// Package logging provides a shared slog JSON logger factory and
// masking helpers for sensitive fields.
package logging

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// logLevel reads LOG_LEVEL once at startup and caches it for the process lifetime.
// Accepted values (case-insensitive): DEBUG, INFO, WARN, ERROR. Default: INFO.
var logLevel = sync.OnceValue(func() slog.Level {
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
})

// NewLogger creates a JSON slog logger writing to stdout,
// pre-seeded with a "service" attribute.
// The minimum log level is controlled by the LOG_LEVEL env var (default INFO).
func NewLogger(service string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	})).With("service", service)
}

// MaskPhone masks a phone number: "0912345678" → "09****78".
func MaskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) <= 4 {
		return strings.Repeat("*", len(phone))
	}
	return phone[:2] + strings.Repeat("*", len(phone)-4) + phone[len(phone)-2:]
}

// MaskAccount masks an account number: "123456789012" → "1234****12".
func MaskAccount(acct string) string {
	if len(acct) <= 6 {
		return strings.Repeat("*", len(acct))
	}
	return acct[:4] + strings.Repeat("*", len(acct)-6) + acct[len(acct)-2:]
}

// getAmountSecret reads LOG_AMOUNT_SECRET once at startup and caches it.
var getAmountSecret = sync.OnceValue(func() []byte {
	s := os.Getenv("LOG_AMOUNT_SECRET")
	if s == "" {
		s = "banking-demo-default"
	}
	return []byte(s)
})

// MaskAmount returns a 12-char hex HMAC-SHA256 of the amount, keyed by LOG_AMOUNT_SECRET.
func MaskAmount(amount int) string {
	mac := hmac.New(sha256.New, getAmountSecret())
	fmt.Fprintf(mac, "%d", amount)
	return fmt.Sprintf("%x", mac.Sum(nil))[:12]
}
