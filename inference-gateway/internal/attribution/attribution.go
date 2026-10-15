// Package attribution is the payload-free request attribution an installation
// gateway sends to the central gateway (DES-HOR-661-03, contract
// docs/architecture/v2-central-inference-gateway.md). Values are opaque
// identifiers, never content. The installation identity itself always comes
// from the client certificate, never from a header.
package attribution

import (
	"net/http"
	"regexp"
)

// Header names.
const (
	HeaderRequestID   = "X-Request-ID"
	HeaderIdentityID  = "X-Iterabase-Identity-Id"
	HeaderRunID       = "X-Iterabase-Run-Id"
	HeaderTurnID      = "X-Iterabase-Turn-Id"
	HeaderWorkflowKey = "X-Iterabase-Workflow-Key"
	HeaderWorkItemID  = "X-Iterabase-Work-Item-Id"
	HeaderChannel     = "X-Iterabase-Channel"
)

// Channels.
const (
	ChannelBrowser = "browser"
	ChannelAPI     = "api"
	ChannelChat    = "chat"
	ChannelMailbox = "mailbox"
	ChannelUnknown = "unknown"
)

// maxLen bounds every attribution value.
const maxLen = 128

var valuePattern = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)

// Attribution is one request's payload-free attribution.
type Attribution struct {
	RequestID   string
	IdentityID  string
	RunID       string
	TurnID      string
	WorkflowKey string
	WorkItemID  string
	Channel     string
}

// Valid reports whether v is an acceptable attribution value.
func Valid(v string) bool { return len(v) <= maxLen && valuePattern.MatchString(v) }

// Channel normalises a channel name; anything unrecognised is unknown.
func Channel(v string) string {
	switch v {
	case ChannelBrowser, ChannelAPI, ChannelChat, ChannelMailbox:
		return v
	}
	return ChannelUnknown
}

// fields pairs each header with its value pointer.
func (a *Attribution) fields() []struct {
	header string
	value  *string
} {
	return []struct {
		header string
		value  *string
	}{
		{HeaderRequestID, &a.RequestID},
		{HeaderIdentityID, &a.IdentityID},
		{HeaderRunID, &a.RunID},
		{HeaderTurnID, &a.TurnID},
		{HeaderWorkflowKey, &a.WorkflowKey},
		{HeaderWorkItemID, &a.WorkItemID},
		{HeaderChannel, &a.Channel},
	}
}

// Header renders the attribution as request headers, skipping empty and
// invalid values.
func (a Attribution) Header() http.Header {
	h := http.Header{}
	if a.Channel != "" {
		a.Channel = Channel(a.Channel)
	}
	for _, f := range a.fields() {
		if *f.value != "" && Valid(*f.value) {
			h.Set(f.header, *f.value)
		}
	}
	return h
}

// FromHeader reads attribution from request headers. Invalid values are
// dropped, not rejected; dropped counts how many were.
func FromHeader(h http.Header) (a Attribution, dropped int) {
	for _, f := range a.fields() {
		v := h.Get(f.header)
		if v == "" {
			continue
		}
		if !Valid(v) {
			dropped++
			continue
		}
		*f.value = v
	}
	if a.Channel != "" {
		a.Channel = Channel(a.Channel)
	}
	return a, dropped
}
