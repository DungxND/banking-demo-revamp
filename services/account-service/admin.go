package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/sm"
	"github.com/stephenafamo/scan"

	internnats "banking-demo/internal/nats"
	"banking-demo/internal/db"
	ilogging "banking-demo/internal/logging"
)

// handleAdminStats returns aggregate counts (users, transfers, notifications) and totals.
// Auth is enforced by RequireAdmin middleware at registration.
// All three aggregate queries run in a single READ COMMITTED read-only transaction
// so they see a consistent snapshot of the database.
// Action: GET /api/account/stats
func handleAdminStats(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, _ json.RawMessage, _ map[string]string) (any, error) {
		type statsRow struct {
			UserCount    int   `db:"user_count"`
			TotalBalance int64 `db:"total_balance"`
		}
		type transferStats struct {
			Count  int   `db:"transfer_count"`
			Volume int64 `db:"total_transfer_amount"`
		}

		var (
			stats      statsRow
			txStats    transferStats
			notifCount int
		)

		// All three queries run inside the same read-only snapshot so that the
		// counts are consistent with each other (no phantom inserts between reads).
		err := bdb.RunInTx(ctx, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context, tx bob.Transaction) error {
			var err error

			// Aggregate user stats: COUNT(*) and COALESCE(SUM(balance),0).
			// Explicit column aliases are required so scan.StructMapper can match db tags.
			stats, err = bob.One(ctx, tx,
				psql.RawQuery(`SELECT COUNT(*) AS user_count, COALESCE(SUM(balance), 0) AS total_balance FROM users`),
				scan.StructMapper[statsRow](),
			)
			if err != nil {
				return fmt.Errorf("query user stats: %w", err)
			}

			txStats, err = bob.One(ctx, tx,
				psql.RawQuery(`SELECT COUNT(*) AS transfer_count, COALESCE(SUM(amount), 0) AS total_transfer_amount FROM transfers`),
				scan.StructMapper[transferStats](),
			)
			if err != nil {
				return fmt.Errorf("query transfer stats: %w", err)
			}

			notifCount, err = bob.One(ctx, tx,
				psql.Select(sm.Columns("COUNT(*)"), sm.From("notifications")),
				scan.SingleColumnMapper[int],
			)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("query admin stats: %w", err)
		}

		logger.Info("admin_stats_request")
		return internnats.Reply(200, map[string]any{
			"user_count":            stats.UserCount,
			"transfer_count":        txStats.Count,
			"total_balance":         stats.TotalBalance,
			"total_transfer_amount": txStats.Volume,
			"total_notifications":   notifCount,
		}), nil
	}
}

// handleAdminUsers returns a paginated, optionally-filtered list of users.
// Supports query params: page, size, search (ILIKE across username/phone/account_number).
//
// Dynamic WHERE is the primary reason bob was chosen — we compose mods at runtime
// rather than building SQL strings or using a second query codepath.
//
// Action: GET /api/account/users
func handleAdminUsers(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		page, size := paginate(raw)

		// Parse optional search term into a targeted struct to avoid an
		// unnecessary map allocation and make the single-field intent clear.
		var search string
		var p struct {
			Search string `json:"search"`
		}
		if err := json.Unmarshal(raw, &p); err == nil {
			search = strings.TrimSpace(p.Search)
		}

		// Build SELECT query with composable mods — WHERE is added only when
		// search is non-empty.
		selectQ := psql.Select(
			sm.Columns(db.UserCols),
			sm.From("users"),
			sm.OrderBy(psql.Quote("id")).Desc(),
			sm.Limit(psql.Arg(size)),
			sm.Offset(psql.Arg((page-1)*size)),
		)
		countQ := psql.Select(
			sm.Columns("COUNT(*)"),
			sm.From("users"),
		)
		if search != "" {
			pattern := "%" + search + "%"
			whereMod := sm.Where(
				psql.Or(
					psql.Quote("username").ILike(psql.Arg(pattern)),
					psql.Quote("phone").ILike(psql.Arg(pattern)),
					psql.Quote("account_number").ILike(psql.Arg(pattern)),
				),
			)
			selectQ.Apply(whereMod)
			countQ.Apply(whereMod)
		}

		users, err := bob.All(ctx, bdb, selectQ, scan.StructMapper[db.User]())
		if err != nil {
			logger.Error("admin_users_query_failed", "error", err.Error(), "page", page, "size", size, "search", search)
			return nil, fmt.Errorf("query users: %w", err)
		}

		total, err := bob.One(ctx, bdb, countQ, scan.SingleColumnMapper[int])
		if err != nil {
			logger.Error("admin_users_count_failed", "error", err.Error(), "page", page, "size", size, "search", search)
			return nil, fmt.Errorf("count users: %w", err)
		}

		result := make([]map[string]any, 0, len(users))
		for _, u := range users {
			result = append(result, userToAdminView(u))
		}

		pages := (total + size - 1) / size
		logger.Info("admin_users_request", "page", page, "size", size, "search", search, "total", total)
		return internnats.Reply(200, map[string]any{
			"users": result,
			"total": total,
			"page":  page,
			"size":  size,
			"pages": pages,
		}), nil
	}
}

// userToAdminView converts a db.User into the admin-facing map representation.
// Phone is masked before serialisation. Extracted as a named function so the
// transformation is independently testable and shared across admin handlers.
func userToAdminView(u db.User) map[string]any {
	return map[string]any{
		"id":             u.ID,
		"phone":          ilogging.MaskPhone(u.Phone),
		"account_number": u.AccountNumber,
		"username":       u.Username,
		"balance":        u.Balance,
		"is_admin":       u.IsAdmin,
	}
}

// transferResponse is the per-row shape returned by handleAdminTransfers.
// A typed struct is used instead of map[string]any to make the JSON contract
// explicit and let the compiler catch field-name typos.
type transferResponse struct {
	ID           int64     `json:"id"`
	FromUser     int64     `json:"from_user"`
	ToUser       int64     `json:"to_user"`
	FromUsername string    `json:"from_username"`
	ToUsername   string    `json:"to_username"`
	Amount       int64     `json:"amount"`
	CreatedAt    time.Time `json:"created_at"`
}

// handleAdminTransfers returns a paginated list of all transfers with sender/receiver usernames.
// Action: GET /api/account/transfers
func handleAdminTransfers(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		page, size := paginate(raw)

		// COUNT first: if the table is empty or the requested page is beyond the
		// last row, skip the page query and the resolveUserNames round-trip entirely.
		total, err := bob.One(ctx, bdb,
			psql.Select(sm.Columns("COUNT(*)"), sm.From("transfers")),
			scan.SingleColumnMapper[int],
		)
		if err != nil {
			return nil, fmt.Errorf("count transfers: %w", err)
		}

		offset := (page - 1) * size
		var transfers []db.Transfer
		var userNames map[int64]string

		if offset < total {
			// Fetch the page only when there are rows to return.
			transfers, err = bob.All(ctx, bdb,
				psql.Select(
					sm.Columns("id", "from_user", "to_user", "amount", "created_at"),
					sm.From("transfers"),
					sm.OrderBy(psql.Quote("id")).Desc(),
					sm.Limit(psql.Arg(size)),
					sm.Offset(psql.Arg(offset)),
				),
				scan.StructMapper[db.Transfer](),
			)
			if err != nil {
				return nil, fmt.Errorf("query transfers: %w", err)
			}

			// Collect user IDs from both sides of every transfer for a single
			// batch username lookup. Deduplication is handled inside resolveUserNames.
			ids := make([]int64, 0, len(transfers)*2)
			for _, t := range transfers {
				ids = append(ids, t.FromUser, t.ToUser)
			}
			userNames, err = resolveUserNames(ctx, bdb, ids)
			if err != nil {
				return nil, err
			}
		}

		result := make([]transferResponse, 0, len(transfers))
		for _, t := range transfers {
			result = append(result, transferResponse{
				ID:           t.ID,
				FromUser:     t.FromUser,
				ToUser:       t.ToUser,
				FromUsername: userNames[t.FromUser],
				ToUsername:   userNames[t.ToUser],
				Amount:       t.Amount,
				CreatedAt:    t.CreatedAt,
			})
		}

		pages := (total + size - 1) / size
		logger.Info("admin_transfers_request", "page", page, "size", size, "total", total)
		return internnats.Reply(200, map[string]any{
			"transfers": result,
			"total":     total,
			"page":      page,
			"size":      size,
			"pages":     pages,
		}), nil
	}
}

// resolveUserNames resolves a slice of user IDs (possibly containing duplicates)
// to a map of id → username using a single WHERE id = ANY($1) query.
// Unknown IDs map to an empty string. Returns an empty map for an empty input.
func resolveUserNames(ctx context.Context, bdb bob.DB, ids []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	// Deduplicate before sending to the database.
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}

	type idName struct {
		ID       int64  `db:"id"`
		Username string `db:"username"`
	}
	rows, err := bob.All(ctx, bdb,
		psql.Select(
			sm.Columns("id", "username"),
			sm.From("users"),
			sm.Where(psql.Quote("id").EQ(psql.Any(psql.Arg(unique)))),
		),
		scan.StructMapper[idName](),
	)
	if err != nil {
		return nil, fmt.Errorf("query usernames: %w", err)
	}
	for _, r := range rows {
		result[r.ID] = r.Username
	}
	return result, nil
}

// handleAdminNotifications returns a paginated list of all notifications.
// Action: GET /api/account/notifications
func handleAdminNotifications(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		page, size := paginate(raw)

		notifs, err := bob.All(ctx, bdb,
			psql.Select(
				sm.Columns("id", "user_id", "message", "is_read", "created_at"),
				sm.From("notifications"),
				sm.OrderBy(psql.Quote("id")).Desc(),
				sm.Limit(psql.Arg(size)),
				sm.Offset(psql.Arg((page-1)*size)),
			),
			scan.StructMapper[db.Notification](),
		)
		if err != nil {
			return nil, fmt.Errorf("query notifications: %w", err)
		}

		total, err := bob.One(ctx, bdb,
			psql.Select(sm.Columns("COUNT(*)"), sm.From("notifications")),
			scan.SingleColumnMapper[int],
		)
		if err != nil {
			return nil, fmt.Errorf("count notifications: %w", err)
		}

		result := make([]map[string]any, 0, len(notifs))
		for _, n := range notifs {
			result = append(result, db.NotificationToMap(n))
		}

		pages := (total + size - 1) / size
		logger.Info("admin_notifications_request", "page", page, "size", size, "total", total)
		return internnats.Reply(200, map[string]any{
			"notifications": result,
			"total":         total,
			"page":          page,
			"size":          size,
			"pages":         pages,
		}), nil
	}
}

// handleAdminUserDetail returns a single user's full profile by ID.
// Action: GET /api/account/user-detail
func handleAdminUserDetail(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		var params map[string]string
		if err := json.Unmarshal(raw, &params); err != nil {
			return internnats.Reply(400, map[string]string{"detail": "invalid payload"}), nil
		}

		userIDStr := strings.TrimSpace(params["user_id"])
		if userIDStr == "" {
			return internnats.Reply(400, map[string]string{"detail": "user_id is required"}), nil
		}
		var targetID int
		if _, err := fmt.Sscan(userIDStr, &targetID); err != nil || targetID <= 0 {
			return internnats.Reply(400, map[string]string{"detail": "user_id must be a positive integer"}), nil
		}

		u, err := bob.One(ctx, bdb,
			psql.Select(
				sm.Columns(db.UserCols),
				sm.From("users"),
				sm.Where(psql.Quote("id").EQ(psql.Arg(targetID))),
			),
			scan.StructMapper[db.User](),
		)
		if db.IsNotFound(err) {
			return internnats.Reply(404, map[string]string{"detail": "User not found"}), nil
		}
		if err != nil {
			return nil, fmt.Errorf("query user detail: %w", err)
		}

		logger.Info("admin_user_detail_request", "target_user_id", targetID)
		return internnats.Reply(200, userToAdminView(u)), nil
	}
}
