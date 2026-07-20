package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/sm"
	"github.com/stephenafamo/bob/dialect/psql/um"
	"github.com/stephenafamo/scan"

	internnats "banking-demo/internal/nats"
	"banking-demo/internal/db"
)

// handleNotifications returns the last 50 notifications for the authenticated user.
// Session is already resolved by RequireSession middleware; user ID is in context.
func handleNotifications(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, _ json.RawMessage, _ map[string]string) (any, error) {
		userID, _ := internnats.UserIDFromContext(ctx)

		notifs, err := bob.All(ctx, bdb,
			psql.Select(
				sm.Columns("id", "user_id", "message", "is_read", "created_at"),
				sm.From("notifications"),
				sm.Where(psql.Quote("user_id").EQ(psql.Arg(userID))),
				sm.OrderBy(psql.Quote("created_at")).Desc(),
				sm.Limit(psql.Arg(50)),
			),
			scan.StructMapper[db.Notification](),
		)
		if db.IsNotFound(err) {
			notifs = nil // treat no rows as empty list
		} else if err != nil {
			logger.Error("notifications_query_failed", "user_id", userID, "error", err.Error())
			return nil, fmt.Errorf("query notifications: %w", err)
		}

		result := make([]map[string]any, 0, len(notifs))
		for _, n := range notifs {
			result = append(result, db.NotificationToMap(n))
		}

		logger.Info("notifications_request", "user_id", userID, "count", len(result))
		return internnats.Reply(200, map[string]any{
			"notifications": result,
		}), nil
	}
}

// ackPayload is the request body for the ack action.
type ackPayload struct {
	ID int64 `json:"id"`
}

// handleAck marks a single notification as read.
// Only the owning user may ack their own notification (user_id guard in the UPDATE).
func handleAck(bdb bob.DB, logger *slog.Logger) internnats.Handler {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ map[string]string) (any, error) {
		userID, _ := internnats.UserIDFromContext(ctx)

		var p ackPayload
		if err := json.Unmarshal(raw, &p); err != nil || p.ID == 0 {
			return internnats.Reply(400, map[string]string{"detail": "invalid payload: id required"}), nil
		}

		res, err := bob.Exec(ctx, bdb,
			psql.Update(
				um.Table("notifications"),
				um.SetCol("is_read").ToArg(true),
				um.Where(psql.Quote("id").EQ(psql.Arg(p.ID))),
				um.Where(psql.Quote("user_id").EQ(psql.Arg(userID))),
			),
		)
		if err != nil {
			logger.Error("ack_failed", "user_id", userID, "notif_id", p.ID, "error", err.Error())
			return nil, fmt.Errorf("ack notification: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Either not found or belongs to another user — return 404 either way
			// to avoid leaking existence of other users' notifications.
			return internnats.Reply(404, map[string]string{"detail": "notification not found"}), nil
		}

		logger.Info("ack_success", "user_id", userID, "notif_id", p.ID)
		return internnats.Reply(200, map[string]string{"status": "ok"}), nil
	}
}
