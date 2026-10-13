package passwordhash

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashMatchesTheControlPlaneFormat(t *testing.T) {
	encoded, err := Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=3,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`).MatchString(encoded) {
		t.Fatalf("hash %q does not match the control plane's encoding", encoded)
	}
	parts := strings.Split(encoded, "$")
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatal(err)
	}
	want := base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte("correct-horse-battery"), salt, 3, 64*1024, 1, 32))
	if parts[5] != want {
		t.Fatal("hash does not verify the password")
	}
	again, err := Hash("correct-horse-battery")
	if err != nil || again == encoded {
		t.Fatal("each hash must use a fresh salt")
	}
}
