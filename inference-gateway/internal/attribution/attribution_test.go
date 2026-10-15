package attribution

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoundTrip(t *testing.T) {
	in := Attribution{RequestID: "req-1", IdentityID: "0b4c3e2a-1f00-4c6b-9a0e-2b7f4a9d1c11", RunID: "r", TurnID: "t",
		WorkflowKey: "invoices/intake", WorkItemID: "wi-1", Channel: ChannelChat}
	out, dropped := FromHeader(in.Header())
	assert.Zero(t, dropped)
	assert.Equal(t, in, out)
}

func TestFromHeader_DropsInvalid(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderRunID, "has space")
	h.Set(HeaderTurnID, strings.Repeat("a", 129))
	h.Set(HeaderWorkflowKey, "ok/key")
	h.Set(HeaderChannel, "telepathy")
	a, dropped := FromHeader(h)
	assert.Equal(t, 2, dropped)
	assert.Empty(t, a.RunID)
	assert.Empty(t, a.TurnID)
	assert.Equal(t, "ok/key", a.WorkflowKey)
	assert.Equal(t, ChannelUnknown, a.Channel)
}

func TestHeader_SkipsEmptyAndInvalid(t *testing.T) {
	h := Attribution{IdentityID: "id-1", RunID: "bad value"}.Header()
	assert.Equal(t, "id-1", h.Get(HeaderIdentityID))
	assert.Empty(t, h.Get(HeaderRunID))
	assert.Len(t, h, 1)
}
