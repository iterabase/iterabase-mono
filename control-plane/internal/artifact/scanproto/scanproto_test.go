package scanproto_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/scanproto"
)

// Only an infected email container may carry children while unaccepted
// (DES-HOR-461-03); a policy rejection never expands.
func TestVerdictChildrenFollowMalwareAndPolicy(t *testing.T) {
	const id, digest, size = "req", "sha256:0000000000000000000000000000000000000000000000000000000000000000", int64(10)
	base := scanproto.Verdict{Version: scanproto.Version, RequestID: id, Digest: digest, Size: size}
	infected := base
	infected.Malware, infected.Reason, infected.Children = scanproto.MalwareInfected, "malware", 3
	assert.NoError(t, infected.Validate(id, digest, size), "an infected container still expands")

	policy := base
	policy.Malware, policy.Reason, policy.Children = scanproto.MalwareClean, "invalid_structure", 1
	assert.Error(t, policy.Validate(id, digest, size), "a policy rejection carries no children")

	tooMany := infected
	tooMany.Children = scanproto.MaxChildren + 1
	assert.Error(t, tooMany.Validate(id, digest, size))

	acceptedInfected := infected
	acceptedInfected.Accepted, acceptedInfected.CanonicalMIME = true, "message/rfc822"
	assert.Error(t, acceptedInfected.Validate(id, digest, size), "infected input is never accepted")
}
