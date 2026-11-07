package config

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Password hashes are written as "pbkdf2-sha256$<iterations>$<salt>$<key>" (base64 without padding).
// Only the standard library is used (no x/crypto), so the algorithm is PBKDF2.
const (
	hashScheme     = "pbkdf2-sha256"
	hashIterations = 600_000
	hashSaltLen    = 16
	hashKeyLen     = 32
)

var b64 = base64.RawStdEncoding

// HashPassword returns the hash of password to store in admin.password_hash.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is empty")
	}
	salt := make([]byte, hashSaltLen)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, hashKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, hashIterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

type passwordHash struct {
	iter      int
	salt, key []byte
}

func parsePasswordHash(s string) (passwordHash, error) {
	bad := fmt.Errorf("admin.password_hash: not a %s$<iterations>$<salt>$<key> hash", hashScheme)
	parts := strings.Split(s, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return passwordHash{}, bad
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return passwordHash{}, bad
	}
	salt, err1 := b64.DecodeString(parts[2])
	key, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) == 0 || len(key) == 0 {
		return passwordHash{}, bad
	}
	return passwordHash{iter, salt, key}, nil
}

// AuthEnabled reports whether /.yard requires logging in with a password.
func (a *Admin) AuthEnabled() bool { return a.PasswordHash != "" }

// CheckPassword reports whether password matches admin.password_hash.
// It is deliberately slow (PBKDF2). It is false when no password is set.
func (a *Admin) CheckPassword(password string) bool {
	h, err := parsePasswordHash(a.PasswordHash)
	if err != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, password, h.salt, h.iter, len(h.key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, h.key) == 1
}
