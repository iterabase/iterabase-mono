package scanner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// SignatureState is the updater's atomically replaced state file on the
// read-only signature volume. CheckedAt is the last time the updater
// confirmed the current bundle against the signature source; freshness is
// measured from it.
type SignatureState struct {
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checkedAt"`
}

// MaxSignatureAge is the DES-HOR-460-05 freshness bound: older signatures
// make the scanner unready and no clean verdict is issued.
const MaxSignatureAge = 24 * time.Hour

// ReadSignatureState reads and bounds the updater state file.
func ReadSignatureState(path string) (SignatureState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return SignatureState{}, fmt.Errorf("read signature state: %w", err)
	}
	if len(b) > 4096 {
		return SignatureState{}, errors.New("signature state too large")
	}
	var st SignatureState
	if err := json.Unmarshal(b, &st); err != nil {
		return SignatureState{}, fmt.Errorf("parse signature state: %w", err)
	}
	if st.Version == "" || len(st.Version) > 128 || st.CheckedAt.IsZero() {
		return SignatureState{}, errors.New("signature state is incomplete")
	}
	return st, nil
}

// Age reports how stale the bundle is at now.
func (s SignatureState) Age(now time.Time) time.Duration { return now.Sub(s.CheckedAt) }
