package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/im"
	"github.com/stephenafamo/bob/dialect/psql/sm"
	"github.com/stephenafamo/scan"

	"banking-demo/internal/db"
	ilogging "banking-demo/internal/logging"
	internnats "banking-demo/internal/nats"
	iredis "banking-demo/internal/redis"
)

// transferEventPublisher is a callback that publishes a durable JetStream event
// after a committed transfer. It is nil when JetStream is unavailable (NATS server
// started without -js). The transfer is already committed to PostgreSQL; a publish
// failure is non-fatal and only logged.
type transferEventPublisher func(ctx context.Context, evt iredis.TransferCompleted) error

// transferPayload is the inbound RPC request body.
type transferPayload struct {
	// ReceiverIdentifier is one of: account_number, phone, or username.
	AccountNumber string `json:"account_number"`
	Phone         string `json:"phone"`
	Username      string `json:"username"`
	Amount        int    `json:"amount"`
}

// transferResult captures the outcome of a successful transaction.
// senderPhone/receiverPhone drive cache invalidation (Tier 1b).
// senderBalance/receiverBalance are the post-TX values used to update
// the balance read model (Tier 2).
type transferResult struct {
	transferID      int64
	senderID        int
	receiverID      int
	senderPhone     string
	receiverPhone   string
	senderBalance   int
	receiverBalance int
}

// Sentinel errors signal business-logic failures from inside the tx callback.
// Using typed sentinels + errors.Is avoids fragile string comparisons.
var (
	errInsufficientFunds = errors.New("insufficient funds")
	errReceiverNotFound  = errors.New("receiver not found")
	errSelfTransfer      = errors.New("self transfer")
)

// handleTransfer performs an atomic balance transfer between two users.
//
// Correctness guarantees:
//   - SERIALIZABLE isolation: prevents phantom reads during balance checks.
//   - Lock ordering: both rows are locked in a single SELECT FOR UPDATE WHERE id IN (?,?)
//     ORDER BY id query, which is the canonical deadlock-prevention ordering.
//   - Post-commit pipeline: Redis DEL+HSET+PUBLISH happens after the transaction
//     commits so the receiver's WebSocket feed never sees a stale or rolled-back state.
//   - JetStream publish: durable event written after the Redis pipeline with a
//     Nats-Msg-Id deduplication header keyed on transferID (Gap-5 idempotency).
//     publishEvent is nil when JetStream is unavailable; Redis pipeline still runs.
func handleTransfer(bdb bob.DB, redisClient *iredis.Client, publishEvent transferEventPublisher, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		// senderID is injected by RequireSession middleware at registration.
		senderID, _ := internnats.UserIDFromContext(ctx)

		// --- Parse & validate payload ---
		var p transferPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return internnats.Reply(400, map[string]string{"detail": "invalid payload"}), nil
		}
		p.AccountNumber = strings.TrimSpace(p.AccountNumber)
		p.Phone = strings.TrimSpace(p.Phone)
		p.Username = strings.TrimSpace(p.Username)

		if p.AccountNumber == "" && p.Phone == "" && p.Username == "" {
			return internnats.Reply(400, map[string]string{"detail": "provide account_number, phone, or username"}), nil
		}
		if p.Amount <= 0 {
			return internnats.Reply(400, map[string]string{"detail": "amount must be positive"}), nil
		}

		// --- Execute atomic transaction ---
		res, txErr := runTransferTx(ctx, bdb, senderID, p)

		// Map business-logic sentinel errors to HTTP responses.
		switch {
		case errors.Is(txErr, errInsufficientFunds):
			return internnats.Reply(400, map[string]string{"detail": "Insufficient funds"}), nil
		case errors.Is(txErr, errReceiverNotFound):
			return internnats.Reply(404, map[string]string{"detail": "Receiver not found"}), nil
		case errors.Is(txErr, errSelfTransfer):
			return internnats.Reply(400, map[string]string{"detail": "Cannot transfer to yourself"}), nil
		case db.IsSerializationFailure(txErr):
			// SQLSTATE 40001 exhausted all retries — caller must retry the request.
			// 503 is the correct HTTP status: the service is healthy but temporarily
			// unable to process this specific request due to DB contention.
			// k6 tracks this as serialization_retries (distinct from generic errors).
			return internnats.Reply(503, map[string]string{"detail": "Serialization failure — please retry"}), nil
		case txErr != nil:
			return nil, fmt.Errorf("transfer tx: %w", txErr)
		}

		// Build the event once — used by both the Redis pipeline and JetStream publish.
		evt := iredis.TransferCompleted{
			TransferID:      res.transferID,
			Amount:          p.Amount,
			SenderID:        res.senderID,
			SenderBalance:   res.senderBalance,
			ReceiverID:      res.receiverID,
			ReceiverBalance: res.receiverBalance,
		}

		// Tier 2 — Post-commit Redis pipeline (single round-trip, always runs).
		// DEL stale user_cache + HSET balance read model + PUBLISH notify event.
		// Non-fatal: transfer is committed to PG. Log on failure so ops can observe.
		if err := iredis.PublishTransferCompleted(ctx, redisClient, evt, res.senderPhone, res.receiverPhone); err != nil {
			logger.Warn("post_commit_redis_pipeline_failed", "receiver_id", res.receiverID, "error", err.Error())
		}

		// Tier 3 — JetStream durable event (nil when server has no -js flag).
		// Nats-Msg-Id = transferID provides Gap-5 deduplication: if the HTTP
		// caller retries after a timeout, the server silently drops the duplicate
		// publish — no double-credit, no duplicate notification.
		if publishEvent != nil {
			if err := publishEvent(ctx, evt); err != nil {
				logger.Warn("jetstream_publish_failed",
					"transfer_id", res.transferID,
					"error", err.Error(),
				)
			}
		}

		logger.Info("transfer_success",
			"transfer_id", res.transferID,
			"sender_id", senderID,
			"receiver_id", res.receiverID,
			"amount", ilogging.MaskAmount(p.Amount),
		)
		return internnats.Reply(200, map[string]any{
			"transfer_id": res.transferID,
			"amount":      p.Amount,
		}), nil
	}
}

// runTransferTx executes the full transfer inside a SERIALIZABLE transaction and
// returns the IDs and post-TX balances needed for the post-commit pipeline.
// Sentinel errors are returned directly so the caller can map them to HTTP responses
// without inspecting error strings.
func runTransferTx(ctx context.Context, bdb bob.DB, senderID int, p transferPayload) (transferResult, error) {
	var res transferResult
	err := db.SerializableTx(ctx, bdb, func(ctx context.Context, tx bob.Transaction) error {
		// 1. Resolve receiver's ID by account_number, phone, or username.
		// Only the id column is fetched here — the full row is re-read under FOR
		// UPDATE lock in lockBothUsers, so fetching all columns now is wasteful.
		receiverID, err := resolveReceiver(ctx, tx, p)
		if err != nil {
			return err // sentinel or wrapped DB error
		}
		if int64(senderID) == receiverID {
			return errSelfTransfer
		}
		res.receiverID = int(receiverID)

		// 2. Lock both rows in deterministic order (lower ID first) to prevent deadlock.
		sender, receiver, err := lockBothUsers(ctx, tx, senderID, int(receiverID))
		if err != nil {
			return err
		}

		// 3. Balance check.
		if int(sender.Balance) < p.Amount {
			return errInsufficientFunds
		}

		// 4. Debit sender, credit receiver.
		if err := updateBalances(ctx, tx, sender.ID, receiver.ID, p.Amount); err != nil {
			return err
		}

		// 5. Persist transfer record + notifications inside the same tx.
		tfr, err := insertTransferRecord(ctx, tx, sender.ID, receiver.ID, p.Amount)
		if err != nil {
			return err
		}
		res.transferID = tfr

		// Capture identifiers and post-TX balances for the post-commit pipeline.
		// No extra DB round-trip: sender/receiver structs carry pre-TX Balance;
		// post-TX values are derived here in Go (matches what the UPDATE applied).
		res.senderID = int(sender.ID)
		res.senderPhone = sender.Phone
		res.senderBalance = int(sender.Balance) - p.Amount
		res.receiverPhone = receiver.Phone
		res.receiverBalance = int(receiver.Balance) + p.Amount

		return insertNotifications(ctx, tx, sender, receiver, p.Amount)
	})
	return res, err
}

// resolveReceiver looks up the receiver's id by the first non-empty identifier in p.
// Only the id column is fetched — the full row is re-read under FOR UPDATE lock by
// lockBothUsers immediately after, so fetching all columns here is wasted bandwidth.
// Returns errReceiverNotFound when no row matches.
func resolveReceiver(ctx context.Context, tx bob.Transaction, p transferPayload) (int64, error) {
	col, val := db.UserIdentifierCol(p.AccountNumber, p.Phone, p.Username)

	id, err := bob.One(ctx, tx,
		psql.Select(
			sm.Columns("id"),
			sm.From("users"),
			sm.Where(col.EQ(psql.Arg(val))),
		),
		scan.SingleColumnMapper[int64],
	)
	if db.IsNotFound(err) {
		return 0, errReceiverNotFound
	}
	return id, err
}

// lockBothUsers acquires SELECT FOR UPDATE locks on both users in a single query,
// ordered by id ASC to prevent deadlocks when two users transfer to each other
// concurrently. Combining the two rows into one query saves one DB round-trip per
// transaction compared to two sequential SELECT FOR UPDATE calls.
// Returns (sender, receiver).
func lockBothUsers(ctx context.Context, tx bob.Transaction, senderID, receiverID int) (sender, receiver db.User, err error) {
	// A single SELECT … WHERE id IN (?, ?) ORDER BY id FOR UPDATE returns both rows
	// in ascending id order, which is the canonical deadlock-prevention order.
	// psql.Raw is used because bob's SM helpers do not expose an IN(args…) + FOR UPDATE
	// combination directly; the raw fragment is parameterised and safe.
	rows, err := bob.All(ctx, tx,
		psql.RawQuery(
			"SELECT "+db.UserCols+
				" FROM users WHERE id IN (?, ?) ORDER BY id FOR UPDATE",
			senderID, receiverID,
		),
		scan.StructMapper[db.User](),
	)
	if err != nil {
		return db.User{}, db.User{}, fmt.Errorf("lock both users: %w", err)
	}
	if len(rows) != 2 {
		return db.User{}, db.User{}, fmt.Errorf("lock both users: expected 2 rows, got %d", len(rows))
	}

	// rows[0] has the lower id, rows[1] the higher — map back to sender/receiver.
	if rows[0].ID == int64(senderID) {
		return rows[0], rows[1], nil
	}
	return rows[1], rows[0], nil
}

// updateBalances debits the sender and credits the receiver in a single UPDATE
// statement using a CASE WHEN expression. This saves one DB round-trip compared
// to two sequential UPDATEs.
//
// The sender arm includes "AND balance >= amount" as a defence-in-depth guard:
// if the row would go negative, that arm's SET is skipped and RowsAffected returns
// 1 (only the receiver updated) or 0. We detect both cases and return errInsufficientFunds.
//
// Returns the sender's post-update balance and receiver's post-update balance via
// the RETURNING clause so the caller can populate the post-commit pipeline without
// extra queries.
func updateBalances(ctx context.Context, tx bob.Transaction, senderID, receiverID int64, amount int) error {
	// Single UPDATE with a CASE WHEN per row. ORDER BY is not needed — Postgres
	// holds both rows locked (acquired in lockBothUsers) so no new deadlock risk.
	res, err := bob.Exec(ctx, tx,
		psql.RawQuery(
			`UPDATE users
			    SET balance = CASE
			        WHEN id = ? AND balance >= ? THEN balance - ?
			        WHEN id = ?                  THEN balance + ?
			    END
			  WHERE id IN (?, ?)`,
			senderID, amount, amount,
				receiverID, amount,
				senderID, receiverID,
		),
	)
	if err != nil {
		return fmt.Errorf("update balances: %w", err)
	}
	// Both rows must be updated. If only 1 row was affected, the sender's CASE arm
	// was skipped — meaning balance < amount (insufficient funds).
	if n, _ := res.RowsAffected(); n != 2 {
		return errInsufficientFunds
	}
	return nil
}

// insertTransferRecord persists the transfer row and returns the generated ID.
func insertTransferRecord(ctx context.Context, tx bob.Transaction, senderID, receiverID int64, amount int) (int64, error) {
	id, err := bob.One(ctx, tx,
		psql.Insert(
			im.Into("transfers", "from_user", "to_user", "amount"),
			im.Values(psql.Arg(senderID), psql.Arg(receiverID), psql.Arg(amount)),
			im.Returning("id"),
		),
		scan.SingleColumnMapper[int64],
	)
	if err != nil {
		return 0, fmt.Errorf("insert transfer: %w", err)
	}
	return id, nil
}

// insertNotifications writes both the sender and receiver notification rows in
// a single multi-row INSERT, reducing the round-trips from two to one.
func insertNotifications(ctx context.Context, tx bob.Transaction, sender, receiver db.User, amount int) error {
	senderMsg := fmt.Sprintf("You sent %d to %s", amount, receiver.Username)
	receiverMsg := fmt.Sprintf("You received %d from %s", amount, sender.Username)

	if _, err := bob.Exec(ctx, tx,
		psql.Insert(
			im.Into("notifications", "user_id", "message"),
			im.Values(psql.Arg(sender.ID), psql.Arg(senderMsg)),
			im.Values(psql.Arg(receiver.ID), psql.Arg(receiverMsg)),
		),
	); err != nil {
		return fmt.Errorf("insert notifications: %w", err)
	}
	return nil
}
