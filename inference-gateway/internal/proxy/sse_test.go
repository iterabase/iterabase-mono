package proxy

import (
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
)

// One byte per read puts every CR at the buffer's end, so a CRLF must not
// split into a line and an empty line (which would dispatch early).
func TestSSEReader_OneByteReads(t *testing.T) {
	stream := ": ping\r\ndata:a\r\ndata: b\r\n\r\nevent: error\rdata: c\r\revent: x\n\ndata: d\n\ndata: unterminated\n"
	r := newSSEReader(iotest.OneByteReader(strings.NewReader(stream)))

	var got []sseEvent
	for {
		ev, ok := r.next()
		if !ok {
			break
		}
		got = append(got, ev)
	}

	assert.NoError(t, r.err())
	assert.Equal(t, []sseEvent{
		{comment: true},
		{data: "a\nb"},
		{name: "error", data: "c"},
		{data: "d"}, // the data-less "x" event dispatches nothing and resets the name
	}, got, "the unterminated final event is discarded")
}
