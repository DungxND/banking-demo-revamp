// Package redis wraps go-redis to provide session management,
// user caching, presence tracking, and pub/sub for notifications.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	instaredis "github.com/instana/go-sensor/instrumentation/instaredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"banking-demo/internal/tracing"
)

// Client is a type alias for go-redis Client, re-exported so service handlers
// can type their parameters as *iredis.Client without importing go-redis directly.
type Client = goredis.Client

// ErrUnauthorized is returned when a session is missing or expired.
var ErrUnauthorized = errors.New("unauthorized")

// onceDuration returns a lazy getter that reads an integer-seconds env var on
// first call (via sync.Once) and returns the cached duration on every subsequent call.
// Use it to declare package-level TTL getters without repeating the sync.Once + var pair.
func onceDuration(envKey string, defaultVal time.Duration) func() time.Duration {
	var (
		once  sync.Once
		value time.Duration
	)
	return func() time.Duration {
		once.Do(func() {
			value = defaultVal
			if s := os.Getenv(envKey); s != "" {
				if n, err := strconv.Atoi(s); err == nil && n > 0 {
					value = time.Duration(n) * time.Second
				}
			}
		})
		return value
	}
}

// TTL getters — each env var is read once and cached for the process lifetime.
var (
	sessionTTL   = onceDuration("SESSION_TTL_SECONDS", 24*time.Hour)
	userCacheTTL = onceDuration("USER_CACHE_TTL_SECONDS", 5*time.Minute)
	presenceTTL  = onceDuration("PRESENCE_TTL_SECONDS", 60*time.Second)
)

// NewClient creates a go-redis client from a Redis URL.
//
// Added in v9.18.0: ConnMaxLifetimeJitter spreads connection expiry across a
// ±30 s window to prevent a thundering-herd on pool refresh.
// Added in v9.19.0: DialerRetryBackoff replaces the constant 100 ms dial
// timeout with exponential back-off (100 ms → 5 s cap) so startup races
// against a not-yet-ready Redis don't busy-spin.
func NewClient(url string) (*goredis.Client, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Spread pool connection expiry to avoid simultaneous reconnects.
	opts.ConnMaxLifetime = 30 * time.Minute
	opts.ConnMaxLifetimeJitter = 30 * time.Second
	// Exponential back-off on dial retries (attempt 0 = delay after 1st fail).
	opts.DialerRetryBackoff = goredis.DialRetryBackoffExponential(100*time.Millisecond, 5*time.Second)
	// Pin the client name to the bare hostname so the instaredis span destination
	// label is "redis" rather than "redis:6379". This aligns the client-span entity
	// with the sensor-discovered entity in the Instana dependency graph, preventing
	// duplicate Redis nodes (redis / redis:6379 / banking-dung-banking-redis).
	if opts.ClientName == "" {
		host, _, _ := strings.Cut(opts.Addr, ":")
		opts.ClientName = host
	}
	client := goredis.NewClient(opts)

	// Attach Instana Redis tracing hook when the collector has been initialised
	// (i.e. tracing.Init was called before NewClient, which is always the case
	// in service main.go). This adds native Instana Redis spans — every GET/SET/
	// PUBLISH/HSET etc. is visible in the Instana dependency map and trace view
	// without any OTel span mapping.
	// WrapClient calls client.AddHook internally and returns the same pointer
	// as an interface — the original *goredis.Client is still valid to use.
	if c := tracing.Collector(); c != nil {
		instaredis.WrapClient(client, c)
	}

	return client, nil
}

// CreateSession stores userID under session:{sid} and returns the session ID.
func CreateSession(ctx context.Context, c *goredis.Client, userID int) (string, error) {
	sid := uuid.NewString()
	if err := c.Set(ctx, "session:"+sid, strconv.Itoa(userID), sessionTTL()).Err(); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return sid, nil
}

// DeleteSession removes a session token so the holder can no longer authenticate.
// Idempotent: deleting a non-existent session returns nil.
func DeleteSession(ctx context.Context, c *goredis.Client, sid string) error {
	if sid == "" {
		return nil
	}
	return c.Del(ctx, "session:"+sid).Err()
}

// GetUserIDFromSession returns the user ID for a session token.
// Returns ErrUnauthorized when the session is absent or expired.
func GetUserIDFromSession(ctx context.Context, c *goredis.Client, sid string) (int, error) {
	if sid == "" {
		return 0, ErrUnauthorized
	}
	val, err := c.Get(ctx, "session:"+sid).Result()
	if errors.Is(err, goredis.Nil) {
		return 0, ErrUnauthorized
	}
	if err != nil {
		return 0, fmt.Errorf("get session: %w", err)
	}
	id, err := strconv.Atoi(val)
	if err != nil {
		return 0, ErrUnauthorized
	}
	return id, nil
}

// CachedUser is the subset stored in Redis for login lookups.
type CachedUser struct {
	ID            int    `json:"id"`
	Phone         string `json:"phone"`
	Username      string `json:"username"`
	AccountNumber string `json:"account_number"`
	PasswordHash  string `json:"password_hash"`
	Balance       int    `json:"balance"`
	IsAdmin       bool   `json:"is_admin"`
}

// SetUserCache stores a CachedUser under both phone and username keys.
func SetUserCache(ctx context.Context, c *goredis.Client, u CachedUser) error {
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("marshal user cache: %w", err)
	}
	ttl := userCacheTTL()
	if err := c.Set(ctx, "user_cache:phone:"+u.Phone, data, ttl).Err(); err != nil {
		return fmt.Errorf("set user cache phone: %w", err)
	}
	if u.Username != "" {
		if err := c.Set(ctx, "user_cache:username:"+u.Username, data, ttl).Err(); err != nil {
			return fmt.Errorf("set user cache username: %w", err)
		}
	}
	return nil
}

// GetUserCache retrieves a CachedUser by key (e.g. "phone:0912345678").
// Returns nil, nil when the key is absent.
//
// Uses GetToBuffer (v9.21.0) with a 512-byte stack buffer to avoid the
// intermediate string allocation that Get().Result() would produce.
// CachedUser JSON is well under 512 bytes; if somehow larger, the error
// is propagated and the caller falls through to a DB fetch.
func GetUserCache(ctx context.Context, c *goredis.Client, key string) (*CachedUser, error) {
	var buf [512]byte
	cmd := c.GetToBuffer(ctx, "user_cache:"+key, buf[:])
	if errors.Is(cmd.Err(), goredis.Nil) {
		return nil, nil
	}
	if cmd.Err() != nil {
		return nil, fmt.Errorf("get user cache: %w", cmd.Err())
	}
	var u CachedUser
	if err := json.Unmarshal(cmd.Bytes(), &u); err != nil {
		return nil, fmt.Errorf("unmarshal user cache: %w", err)
	}
	return &u, nil
}

// GetUserCacheByPhone looks up a CachedUser by phone number.
// Returns nil, nil on a cache miss. Callers never construct the key format.
func GetUserCacheByPhone(ctx context.Context, c *goredis.Client, phone string) (*CachedUser, error) {
	return GetUserCache(ctx, c, "phone:"+phone)
}

// GetUserCacheByUsername looks up a CachedUser by username.
// Returns nil, nil on a cache miss. Callers never construct the key format.
func GetUserCacheByUsername(ctx context.Context, c *goredis.Client, username string) (*CachedUser, error) {
	return GetUserCache(ctx, c, "username:"+username)
}

// PresenceTTL returns the configured presence TTL. Notification-service uses
// this to derive its heartbeat interval so the invariant is always satisfied.
// Override with PRESENCE_TTL_SECONDS env var (default 60 s).
func PresenceTTL() time.Duration { return presenceTTL() }

// SetPresence writes a presence key with the configured TTL when online is true,
// or deletes the key when online is false.
func SetPresence(ctx context.Context, c *goredis.Client, userID int, online bool) error {
	key := fmt.Sprintf("presence:%d", userID)
	if online {
		return c.Set(ctx, key, "online", presenceTTL()).Err()
	}
	return c.Del(ctx, key).Err()
}

// NotifyEvent is the typed payload published to notify:{userID} channels.
// Using a struct prevents format drift between the publisher (transfer-service)
// and the subscriber (notification-service ws.go).
type NotifyEvent struct {
	TransferID int64 `json:"transfer_id"`
	Amount     int   `json:"amount"`
}

// TransferCompleted is the richer event written by the Tier-2 post-commit
// pipeline. It carries post-TX balances so the balance read model can be
// updated in the same Redis round-trip as the cache invalidation.
type TransferCompleted struct {
	TransferID      int64 `json:"transfer_id"`
	Amount          int   `json:"amount"`
	SenderID        int   `json:"sender_id"`
	SenderBalance   int   `json:"sender_balance"`   // post-TX
	ReceiverID      int   `json:"receiver_id"`
	ReceiverBalance int   `json:"receiver_balance"` // post-TX
}

// SetBalance writes the post-TX balance for a single user into the "balance"
// Redis Hash (field = userID string). Called as part of the post-commit pipeline
// in transfer-service; safe to call standalone for cold-start warm-ups.
func SetBalance(ctx context.Context, c *goredis.Client, userID, balance int) error {
	return c.HSet(ctx, "balance", strconv.Itoa(userID), balance).Err()
}

// SetBalanceBatch writes post-TX balances for two users (sender and receiver)
// into the "balance" Redis Hash in a single pipeline round-trip.
// Use this from balance-projection consumers and post-commit pipelines wherever
// both sides of a transfer must be updated atomically in Redis.
// All Redis key strings are owned here; callers work only in domain terms.
func SetBalanceBatch(ctx context.Context, c *goredis.Client, senderID, senderBalance, receiverID, receiverBalance int) error {
	pipe := c.Pipeline()
	pipe.HSet(ctx, "balance", senderID, senderBalance)
	pipe.HSet(ctx, "balance", receiverID, receiverBalance)
	_, err := pipe.Exec(ctx)
	return err
}

// GetBalance reads a user's balance from the "balance" Redis Hash.
// Returns (balance, true, nil) on a hit, (0, false, nil) on a cache miss
// (goredis.Nil absorbed), and (0, false, err) on any other Redis error.
// Callers never need to import go-redis to distinguish a miss from an error.
func GetBalance(ctx context.Context, c *goredis.Client, userID int) (int, bool, error) {
	val, err := c.HGet(ctx, "balance", strconv.Itoa(userID)).Int()
	if errors.Is(err, goredis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return val, true, nil
}

// PublishTransferCompleted executes the post-commit pipeline in a single
// Redis round-trip:
//  1. DEL user_cache:phone:{senderPhone} and user_cache:phone:{receiverPhone}
//     — invalidates stale CachedUser entries (Tier 1b cache fix).
//  2. HSET balance {senderID} {senderBalance}
//     HSET balance {receiverID} {receiverBalance}
//     — updates the balance read model (Tier 2).
//  3. PUBLISH notify:{senderID}   — real-time WebSocket event for the sender.
//     PUBLISH notify:{receiverID} — real-time WebSocket event for the receiver.
//
// All Redis key names are owned here; callers work only in domain terms.
func PublishTransferCompleted(ctx context.Context, c *goredis.Client, evt TransferCompleted, senderPhone, receiverPhone string) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal transfer completed: %w", err)
	}
	pipe := c.Pipeline()
	pipe.Del(ctx,
		"user_cache:phone:"+senderPhone,
		"user_cache:phone:"+receiverPhone,
	)
	pipe.HSet(ctx, "balance", evt.SenderID, evt.SenderBalance)
	pipe.HSet(ctx, "balance", evt.ReceiverID, evt.ReceiverBalance)
	pipe.Publish(ctx, fmt.Sprintf("notify:%d", evt.SenderID), string(data))
	pipe.Publish(ctx, fmt.Sprintf("notify:%d", evt.ReceiverID), string(data))
	_, err = pipe.Exec(ctx)
	return err
}

// PublishNotify marshals event to JSON and publishes it to notify:{userID}.
func PublishNotify(ctx context.Context, c *goredis.Client, userID int, event NotifyEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal notify event: %w", err)
	}
	return c.Publish(ctx, fmt.Sprintf("notify:%d", userID), string(data)).Err()
}

// Subscribe subscribes to a Redis channel and returns a receive-only string
// channel plus an unsubscribe function. The caller must call unsubscribe when done.
func Subscribe(ctx context.Context, c *goredis.Client, channel string) (<-chan string, func()) {
	sub := c.Subscribe(ctx, channel)
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		for msg := range sub.Channel() {
			select {
			case ch <- msg.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, func() { _ = sub.Close() }
}

// SubscribeNotify subscribes to the notify:{userID} channel for a given user.
// It owns the channel name format; callers work in domain terms only.
// Returns the same (ch, unsubscribe) pair as Subscribe.
func SubscribeNotify(ctx context.Context, c *goredis.Client, userID int) (<-chan string, func()) {
	return Subscribe(ctx, c, fmt.Sprintf("notify:%d", userID))
}
