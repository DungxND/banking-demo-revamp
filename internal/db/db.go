// Package db provides a pgx connection pool, typed row structs,
// a bob.DB adapter, and query helpers shared across all services.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	instapgx "github.com/instana/go-sensor/instrumentation/instapgx/v2"
	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/psql"
	"github.com/stephenafamo/bob/dialect/psql/sm"
	"github.com/stephenafamo/scan"

	"banking-demo/internal/tracing"
)

// pgErrCode extracts the PostgreSQL SQLSTATE code from an error, returning ""
// when the error is not a *pgconn.PgError.
func pgErrCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// User mirrors the users table.
// Fields use int64 to match PostgreSQL BIGINT (migration 000003).
type User struct {
	ID            int64  `db:"id"`
	Phone         string `db:"phone"`
	AccountNumber string `db:"account_number"`
	Username      string `db:"username"`
	PasswordHash  string `db:"password_hash"`
	Balance       int64  `db:"balance"`
	IsAdmin       bool   `db:"is_admin"`
}

// Transfer mirrors the transfers table.
type Transfer struct {
	ID        int64     `db:"id"`
	FromUser  int64     `db:"from_user"`
	ToUser    int64     `db:"to_user"`
	Amount    int64     `db:"amount"`
	CreatedAt time.Time `db:"created_at"`
}

// Notification mirrors the notifications table.
type Notification struct {
	ID        int64     `db:"id"`
	UserID    int64     `db:"user_id"`
	Message   string    `db:"message"`
	IsRead    bool      `db:"is_read"`
	CreatedAt time.Time `db:"created_at"`
}

// ErrNotFound is returned when a query returns no rows.
// Use IsNotFound to check for it.
var ErrNotFound = sql.ErrNoRows

// IsNotFound reports whether err signals a missing row.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// IsUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505). Use this instead of inspecting err.Error()
// for the substring "unique" — the error code is stable across driver versions.
func IsUniqueViolation(err error) bool {
	return pgErrCode(err) == "23505"
}

// IsSerializationFailure reports whether err is a PostgreSQL serialization
// failure (SQLSTATE 40001). SERIALIZABLE transactions must be retried when
// this error occurs; it is not a permanent failure.
func IsSerializationFailure(err error) bool {
	return pgErrCode(err) == "40001"
}

// UserCols is the canonical SELECT column list for the users table.
// Exported so service handlers and sub-functions can reference it without
// repeating the column list, preventing drift between query sites.
const UserCols = "id, phone, account_number, username, password_hash, balance, is_admin"

// UserIdentifierCol returns the psql column expression and value for the first
// non-empty identifier, in priority order: account_number > phone > username.
// Used by account-service and transfer-service to resolve a receiver by any identifier.
func UserIdentifierCol(accountNumber, phone, username string) (psql.Expression, string) {
	switch {
	case accountNumber != "":
		return psql.Quote("account_number"), accountNumber
	case phone != "":
		return psql.Quote("phone"), phone
	default:
		return psql.Quote("username"), username
	}
}

// NotificationToMap converts a Notification row to the JSON-serialisable map
// shape returned by the notifications API. Shared by notification-service and
// account-service (admin) to prevent field-name drift between the two paths.
func NotificationToMap(n Notification) map[string]any {
	return map[string]any{
		"id":         n.ID,
		"user_id":    n.UserID,
		"message":    n.Message,
		"is_read":    n.IsRead,
		"created_at": n.CreatedAt,
	}
}

// QueryUser fetches a single user by an arbitrary column filter.
// col must be a psql.Expression (e.g. psql.Quote("id")), val is bound as an arg.
// Returns ErrNotFound (= sql.ErrNoRows) when no row matches.
func QueryUser(ctx context.Context, exec bob.Executor, col psql.Expression, val any) (User, error) {
	return bob.One(ctx, exec,
		psql.Select(
			sm.Columns(UserCols),
			sm.From("users"),
			sm.Where(col.EQ(psql.Arg(val))),
		),
		scan.StructMapper[User](),
	)
}

// QueryUserForUpdate is like QueryUser but acquires a row-level lock (SELECT FOR UPDATE).
// Must be called inside a transaction.
func QueryUserForUpdate(ctx context.Context, exec bob.Executor, col psql.Expression, val any) (User, error) {
	return bob.One(ctx, exec,
		psql.Select(
			sm.Columns(UserCols),
			sm.From("users"),
			sm.Where(col.EQ(psql.Arg(val))),
			sm.ForUpdate(),
		),
		scan.StructMapper[User](),
	)
}

// NewPool creates a pgxpool from the provided DSN (or DATABASE_URL env var if dsn is empty).
// MaxConns defaults to 15; override with DB_POOL_SIZE env var.
//
// PostgreSQL skill connection settings applied:
//   - MinConns 2: keep warm connections ready to avoid cold-start latency.
//   - MaxConnLifetime 30 min: recycle connections before server-side idle timeout.
//   - MaxConnIdleTime 5 min: release idle connections faster than max lifetime.
//   - HealthCheckPeriod 1 min: detect broken pool connections proactively.
//   - idle_in_transaction_session_timeout 30 s: prevents connections stuck in
//     BEGIN that block VACUUM from reclaiming dead tuples across the entire DB.
//   - statement_timeout 30 s: kills runaway queries before they starve
//     the connection pool; override with STATEMENT_TIMEOUT_MS env var.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	if s := os.Getenv("DB_POOL_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			cfg.MaxConns = int32(n)
		}
	} else {
		cfg.MaxConns = 15
	}

	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	// Session-level guards set at connection time so they apply to every backend
	// regardless of which service acquires the connection.
	stmtTimeoutMs := "30000"
	if s := os.Getenv("STATEMENT_TIMEOUT_MS"); s != "" {
		stmtTimeoutMs = s
	}
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30000" // 30 s
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = stmtTimeoutMs

	// Attach Instana pgx tracer when the collector has been initialised (i.e.
	// tracing.Init was called before NewPool, which is always the case in
	// service main.go). This enables native Instana DB spans — SQL statements
	// are visible in the Instana dependency map without OTel span mapping.
	if c := tracing.Collector(); c != nil {
		cfg.ConnConfig.Tracer = instapgx.InstanaTracer(cfg.ConnConfig, c)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// LogPoolStatus logs pgxpool statistics at startup and on-demand.
// Reports max_conns, total_conns, idle_conns, acquired_conns so that
// operators can immediately spot pool exhaustion or misconfiguration.
// The db performance skill: "If WaitCount keeps climbing, increase
// MaxOpenConns or optimize slow queries."
func LogPoolStatus(pool *pgxpool.Pool, logger *slog.Logger) {
	s := pool.Stat()
	logger.Info("db_pool_ready",
		"max_conns", s.MaxConns(),
		"total_conns", s.TotalConns(),
		"idle_conns", s.IdleConns(),
		"acquired_conns", s.AcquiredConns(),
		"new_conns_count", s.NewConnsCount(),
		"max_lifetime_destroy_count", s.MaxLifetimeDestroyCount(),
		"max_idle_destroy_count", s.MaxIdleDestroyCount(),
	)
}

// NewBobDB wraps a pgxpool.Pool as a bob.DB using pgx/v5/stdlib as the bridge.
// Create once at startup and reuse for the lifetime of the process.
func NewBobDB(pool *pgxpool.Pool) bob.DB {
	return bob.NewDB(stdlib.OpenDBFromPool(pool))
}

// maxSerializableRetries caps the number of automatic retries for serialization
// failures (SQLSTATE 40001) in SERIALIZABLE transactions.
const maxSerializableRetries = 5

// SerializableTx runs fn inside a SERIALIZABLE transaction via bob.DB.RunInTx.
// Required by the transfer service to prevent phantom reads during balance checks.
//
// PostgreSQL can abort a SERIALIZABLE transaction with SQLSTATE 40001
// ("could not serialize access due to concurrent update") even when no logical
// error occurred — the correct response is to retry the entire transaction.
// This wrapper retries up to maxSerializableRetries times before returning the
// error to the caller.
//
// Between retries the caller sleeps for a random duration in [0, 2^attempt × 2ms)
// (full-jitter exponential backoff). This staggers simultaneous retries from
// competing goroutines, reducing the probability that they collide again on the
// next attempt. The last attempt skips the sleep to minimise tail latency.
func SerializableTx(ctx context.Context, bdb bob.DB, fn func(context.Context, bob.Transaction) error) error {
	opts := &sql.TxOptions{Isolation: sql.LevelSerializable}
	for attempt := range maxSerializableRetries {
		err := bdb.RunInTx(ctx, opts, fn)
		if err == nil {
			return nil
		}
		// Only retry on PostgreSQL serialization failure (40001).
		// Any other error — including sentinel business errors — is permanent.
		if !IsSerializationFailure(err) {
			return err
		}
		// Last attempt — return immediately without sleeping.
		if attempt == maxSerializableRetries-1 {
			return fmt.Errorf("serializable tx failed after %d attempts: %w", maxSerializableRetries, err)
		}
		// Full-jitter exponential backoff: sleep [0, 2^attempt × 2ms).
		// Cap at 100ms to bound worst-case added latency.
		maxDelay := time.Duration(1<<uint(attempt)) * 2 * time.Millisecond
		if maxDelay > 100*time.Millisecond {
			maxDelay = 100 * time.Millisecond
		}
		jitter := time.Duration(rand.Int64N(int64(maxDelay)))
		select {
		case <-ctx.Done():
			return fmt.Errorf("serializable tx cancelled during retry backoff: %w", ctx.Err())
		case <-time.After(jitter):
		}
	}
	// Unreachable — loop always returns above.
	return nil
}
