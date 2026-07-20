package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	iredis "banking-demo/internal/redis"
)

// presenceHeartbeatInterval is derived from the Redis presence TTL so that the
// heartbeat always fires before the key expires, regardless of how PRESENCE_TTL_SECONDS
// is configured. Using TTL/3 gives two missed ticks before the user appears offline.
func presenceHeartbeatInterval() time.Duration {
	return iredis.PresenceTTL() / 3
}

// wsHandler upgrades the HTTP connection to WebSocket, then:
//  1. Authenticates via `?session=<sid>` query param.
//  2. Marks user as online (presence heartbeat at presenceHeartbeatInterval).
//  3. Subscribes to the user's Redis notify channel and forwards every
//     message as a JSON WebSocket frame.
//  4. On disconnect: marks user offline and unsubscribes.
func wsHandler(redisClient *iredis.Client, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.URL.Query().Get("session")
		if sid == "" {
			http.Error(w, `{"detail":"missing session"}`, http.StatusUnauthorized)
			return
		}

		// Validate session before upgrading — avoids wasting a WS connection on bad tokens.
		userID, err := iredis.GetUserIDFromSession(r.Context(), redisClient, sid)
		if errors.Is(err, iredis.ErrUnauthorized) {
			http.Error(w, `{"detail":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, `{"detail":"session error"}`, http.StatusInternalServerError)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Allow any origin; production deployments restrict via CORS middleware upstream.
			InsecureSkipVerify: true,
		})
		if err != nil {
			// websocket.Accept already wrote the error response; just log and return.
			logger.Info("ws_upgrade_failed", "user_id", userID, "error", err.Error())
			return
		}
		defer conn.CloseNow()

		logger.Info("ws_connected", "user_id", userID)

		// Per-connection context: cancelled when the WS closes or the server shuts down.
		// CloseRead drains incoming frames (ping/pong/close) internally and cancels the
		// returned context when the client sends a close frame or the connection drops.
		// This is the correct pattern for write-only connections per the coder/websocket docs.
		wsCtx := conn.CloseRead(r.Context())

		msgCh, unsub := iredis.SubscribeNotify(wsCtx, redisClient, userID)
		defer unsub()

		// disconnect closes the WebSocket cleanly, marks the user offline, and logs the event.
		// Uses context.Background() for SetPresence because wsCtx is already cancelled on disconnect.
		disconnect := func() {
			conn.Close(websocket.StatusNormalClosure, "")
			_ = iredis.SetPresence(context.Background(), redisClient, userID, false)
			logger.Info("ws_disconnected", "user_id", userID)
		}

		// Mark user online immediately, then refresh at presenceHeartbeatInterval()
		// (PresenceTTL()/3) — heartbeat always fires before the key expires.
		go func() {
			_ = iredis.SetPresence(wsCtx, redisClient, userID, true)
			ticker := time.NewTicker(presenceHeartbeatInterval())
			defer ticker.Stop()
			for {
				select {
				case <-wsCtx.Done():
					return
				case <-ticker.C:
					_ = iredis.SetPresence(wsCtx, redisClient, userID, true)
				}
			}
		}()

		// Pump Redis messages → WebSocket in the main goroutine.
		// wsCtx is already cancelled when the client disconnects (via CloseRead),
		// so this loop exits naturally on disconnect or server shutdown.
		for {
			select {
			case <-wsCtx.Done():
				disconnect()
				return
			case msg, ok := <-msgCh:
				if !ok {
					disconnect()
					return
				}
				// msg is the raw JSON published to the notify channel (e.g. TransferCompleted).
				// Deliver it as a structured JSON object by unmarshaling into a raw message
				// so the WS frame is {"event":{...}} rather than {"event":"{\\"...\\"}"}
				// (the latter double-encodes the payload as an escaped string).
				var raw json.RawMessage
				if err := json.Unmarshal([]byte(msg), &raw); err != nil {
					// Malformed publish payload — forward as string so nothing is silently dropped.
					raw, _ = json.Marshal(msg)
				}
				payload := map[string]json.RawMessage{"event": raw}
				if writeErr := wsjson.Write(wsCtx, conn, payload); writeErr != nil {
					if !errors.Is(writeErr, context.Canceled) {
						logger.Info("ws_write_failed", "user_id", userID, "error", writeErr.Error())
					}
					disconnect()
					return
				}
			}
		}
	}
}
