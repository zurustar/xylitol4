package userdb

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// ComputeHA1 normalises a stored password value into the HA1 digest defined by
// RFC 7616. The input may already be a 32 character hexadecimal digest or a
// plaintext password. When plaintext is provided the return value is the MD5 of
// username:realm:password, matching the behaviour expected by the registrar.
func ComputeHA1(username, realm, stored string) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return ""
	}
	if len(stored) == 32 && isHex(stored) {
		return strings.ToLower(stored)
	}
	return HashPassword(username, realm, stored)
}

// HashPassword returns the HA1 digest for the provided plaintext password.
func HashPassword(username, realm, password string) string {
	raw := fmt.Sprintf("%s:%s:%s", username, realm, password)
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// VerifyPassword reports whether the provided plaintext password matches the
// stored value for the given identity. Empty passwords only match when the
// stored credentials are also empty.
func VerifyPassword(stored, username, realm, candidate string) bool {
	storedHash := ComputeHA1(username, realm, stored)
	if strings.TrimSpace(candidate) == "" {
		return storedHash == ""
	}
	candidateHash := HashPassword(username, realm, candidate)
	return subtleConstantTimeCompare(storedHash, candidateHash)
}

func subtleConstantTimeCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func isHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
