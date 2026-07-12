// Package auth provides password hashing and verification using bcrypt.
package auth

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/crypto/bcrypt"
)

// bcryptRounds returns the bcrypt cost factor from BCRYPT_ROUNDS env (range 4–31, default 10).
// Invalid or out-of-range values are silently ignored and the default is used.
func bcryptRounds() int {
	if s := os.Getenv("BCRYPT_ROUNDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 4 && n <= 31 {
			return n
		}
	}
	return 10
}

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptRounds())
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword reports whether pw matches the stored bcrypt hash.
func VerifyPassword(pw, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
