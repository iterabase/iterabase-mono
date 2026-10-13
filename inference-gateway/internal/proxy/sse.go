package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// sseEvent is one dispatched provider event, or a comment line.
type sseEvent struct {
	name    string
	data    string
	comment bool
}

// sseReader assembles a provider's event stream into events the way a browser
// EventSource does (WHATWG HTML, server-sent events): a field's value may
// follow its colon with or without one space, data may span several lines,
// lines end in LF, CRLF or CR, and an event dispatches only at a blank line.
// The gateway decides on whole events, so no framing variant can carry a
// provider error past the check in pieces.
type sseReader struct {
	scanner *bufio.Scanner
	name    string
	data    strings.Builder
}

func newSSEReader(r io.Reader) *sseReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	s.Split(scanSSELines)
	return &sseReader{scanner: s}
}

// next returns the next event or comment. It returns false at the end of the
// stream; an event the stream ended in the middle of is discarded.
func (r *sseReader) next() (sseEvent, bool) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if r.data.Len() == 0 {
				r.name = ""
				continue
			}
			ev := sseEvent{name: r.name, data: strings.TrimSuffix(r.data.String(), "\n")}
			r.name = ""
			r.data.Reset()
			return ev, true
		}
		if strings.HasPrefix(line, ":") {
			return sseEvent{comment: true}, true
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			r.data.WriteString(value)
			r.data.WriteByte('\n')
		case "event":
			r.name = value
		}
		// id and retry are reconnection hints for a browser; the gateway does
		// not reconnect to providers, so they are dropped with unknown fields.
	}
	return sseEvent{}, false
}

func (r *sseReader) err() error { return r.scanner.Err() }

// isMessage reports whether the event has the default type: unnamed, or
// explicitly named "message", which SSE treats identically.
func (ev sseEvent) isMessage() bool { return ev.name == "" || ev.name == "message" }

// scanSSELines splits on LF, CRLF or a lone CR.
func scanSSELines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		switch {
		case i+1 < len(data) && data[i+1] == '\n':
			return i + 2, data[:i], nil
		case i+1 < len(data) || atEOF:
			return i + 1, data[:i], nil
		}
		return 0, nil, nil // a CR at the buffer's end may start a CRLF
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// isCompletionChunk reports whether an event is an OpenAI-style completion
// chunk: an unnamed (or "message") event whose data is a JSON object with no
// error member. Anything else from the provider, including an error envelope
// or an "error" event, is a mid-stream failure whose body is never forwarded.
func isCompletionChunk(ev sseEvent) bool {
	if !ev.isMessage() {
		return false
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ev.data), &parsed); err != nil || parsed == nil {
		return false
	}
	e, ok := parsed["error"]
	return !ok || string(e) == "null"
}
