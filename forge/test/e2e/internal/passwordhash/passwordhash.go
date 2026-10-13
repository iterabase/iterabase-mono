// Package passwordhash encodes the control plane's self-describing Argon2id
// password hash at the approved floor (DES-HOR-451-06: 64 MiB, 3 iterations,
// parallelism 1, 16-byte salt, 32-byte key). Fixture and preview tooling use it
// to seed a known account without the email setup channel; product code never
// does.
package passwordhash

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	memoryKiB   = 64 * 1024
	iterations  = 3
	parallelism = 1
	saltLength  = 16
	keyLength   = 32
)

// Hash returns the encoded Argon2id hash of password with a fresh random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, iterations, memoryKiB, parallelism, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memoryKiB, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
