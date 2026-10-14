package admission

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
)

// pdfNameReasons maps PDF names (after #xx escape decoding) to the reason
// their presence triggers. This is a bounded lexical scanner, not a parser: a
// forbidden name anywhere, including inside strings, rejects fail-closed.
var pdfNameReasons = map[string]string{
	"Encrypt":       ReasonEncrypted,
	"JavaScript":    ReasonActiveContent,
	"JS":            ReasonActiveContent,
	"Launch":        ReasonActiveContent,
	"RichMedia":     ReasonActiveContent,
	"XFA":           ReasonActiveContent,
	"AA":            ReasonActiveContent,
	"EmbeddedFile":  ReasonEmbeddedObject,
	"EmbeddedFiles": ReasonEmbeddedObject,
}

// pdfReasonPriority orders reasons when several names are present.
var pdfReasonPriority = []string{ReasonEncrypted, ReasonActiveContent, ReasonEmbeddedObject}

const (
	pdfMaxNameBytes   = 64
	pdfDictLookBehind = 64 << 10
)

func validatePDF(data []byte) string {
	if !bytes.Contains(head(data, 1024), []byte("%PDF-")) ||
		!bytes.Contains(data[max(0, len(data)-1024):], []byte("%%EOF")) ||
		!bytes.Contains(data, []byte("startxref")) {
		return ReasonMalformed
	}
	found := make(map[string]bool)
	scanPDFNames(data, found)
	// An encrypted document's streams are ciphertext; stop before inflating.
	if found[ReasonEncrypted] {
		return ReasonEncrypted
	}
	if reason := scanPDFStreams(data, found); reason != "" {
		return reason
	}
	for _, reason := range pdfReasonPriority {
		if found[reason] {
			return reason
		}
	}
	return ""
}

func scanPDFNames(data []byte, found map[string]bool) {
	forEachPDFName(data, func(name []byte) {
		if reason, ok := pdfNameReasons[string(name)]; ok {
			found[reason] = true
		}
	})
}

// forEachPDFName calls fn with every decoded name token in data. Names longer
// than pdfMaxNameBytes are consumed but not reported (none can match).
func forEachPDFName(data []byte, fn func([]byte)) {
	name := make([]byte, 0, pdfMaxNameBytes)
	for i := 0; i < len(data); i++ {
		if data[i] != '/' {
			continue
		}
		name = name[:0]
		overflow := false
		j := i + 1
		for ; j < len(data) && !isPDFDelimiter(data[j]); j++ {
			c := data[j]
			if c == '#' && j+2 < len(data) && isHex(data[j+1]) && isHex(data[j+2]) {
				c = unhex(data[j+1])<<4 | unhex(data[j+2])
				j += 2
			}
			if len(name) == pdfMaxNameBytes {
				overflow = true
				continue
			}
			name = append(name, c)
		}
		if !overflow && len(name) > 0 {
			fn(name)
		}
		i = j - 1
	}
}

// scanPDFStreams inflates FlateDecode streams and scans the inflated bytes
// for forbidden names. Hostile-input hardening:
//   - Inflation starts at the stream body and runs to the deflate stream's own
//     end, not to a possibly forged "endstream" or /Length.
//   - A body with a valid zlib header is inflated even when its dictionary
//     cannot be found, so a malformed dictionary cannot hide a stream.
//   - Inflate errors are tolerated and the partial output is scanned, because
//     a lenient reader may still render a truncated stream.
//   - Both compressed input consumed and output produced are charged to one
//     budget, so overlapping or empty-block streams cannot go quadratic.
func scanPDFStreams(data []byte, found map[string]bool) string {
	b := newBudget()
	inflater := newPDFInflater()
	var out bytes.Buffer
	// floor is where the previous stream keyword ended: look-behinds never
	// re-scan earlier bytes, so dictionary matching stays linear.
	floor := 0
	for pos := 0; pos < len(data); {
		at := bytes.Index(data[pos:], []byte("stream"))
		if at < 0 {
			return ""
		}
		keyword := pos + at
		pos = keyword + len("stream")
		start, ok := streamBodyStart(data, pos)
		if !ok {
			continue
		}
		dict, dictFound := streamDict(data, keyword, floor)
		floor = start
		if end := bytes.Index(data[start:], []byte("endstream")); end >= 0 {
			pos = start + end + len("endstream")
		} else {
			pos = len(data)
		}
		declaredFlate := dictFound && hasFlateFilter(dict)
		if !declaredFlate && !hasZlibHeader(data[start:]) {
			continue
		}
		limit := min(int64(MaxPDFStreamBytes), b.remaining)
		out.Reset()
		body := &chargedReader{data: data[start:], b: b}
		n := inflater.inflate(&out, body, declaredFlate, limit+1)
		if body.exhausted || n > MaxPDFStreamBytes || !b.take(n) {
			return ReasonResourceLimit
		}
		scanPDFNames(out.Bytes(), found)
	}
	return ""
}

func streamBodyStart(data []byte, pos int) (int, bool) {
	switch {
	case bytes.HasPrefix(data[pos:], []byte("\r\n")):
		return pos + 2, true
	case pos < len(data) && (data[pos] == '\n' || data[pos] == '\r'):
		return pos + 1, true
	}
	return 0, false
}

// streamDict returns the dictionary that immediately precedes a stream
// keyword, matching nested << >> within a bounded look-behind above floor.
func streamDict(data []byte, keyword, floor int) ([]byte, bool) {
	end := keyword
	for end > 0 && isPDFWhitespace(data[end-1]) {
		end--
	}
	if end < 2 || data[end-2] != '>' || data[end-1] != '>' {
		return nil, false
	}
	lower := max(floor, end-pdfDictLookBehind)
	depth := 0
	for i := end - 1; i > lower; i-- {
		switch {
		case data[i] == '>' && data[i-1] == '>':
			depth++
			i--
		case data[i] == '<' && data[i-1] == '<':
			depth--
			i--
			if depth == 0 {
				return data[i:end], true
			}
		}
	}
	return nil, false
}

func hasFlateFilter(dict []byte) bool {
	flateFound := false
	forEachPDFName(dict, func(name []byte) {
		if string(name) == "FlateDecode" || string(name) == "Fl" {
			flateFound = true
		}
	})
	return flateFound
}

func hasZlibHeader(body []byte) bool {
	return len(body) >= 2 && body[0]&0x0F == 8 && body[0]>>4 <= 7 &&
		(uint16(body[0])<<8|uint16(body[1]))%31 == 0
}

// chargedReader is a byte reader over a stream body that charges every
// compressed byte consumed to the budget and reports EOF once it runs out.
type chargedReader struct {
	data      []byte
	pos       int
	b         *budget
	exhausted bool
}

func (r *chargedReader) Read(p []byte) (int, error) {
	n := int(min(int64(min(len(p), len(r.data)-r.pos)), r.b.remaining))
	if n <= 0 {
		r.exhausted = r.pos < len(r.data)
		return 0, io.EOF
	}
	r.b.take(int64(n))
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *chargedReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if !r.b.take(1) {
		r.exhausted = true
		return 0, io.EOF
	}
	r.pos++
	return r.data[r.pos-1], nil
}

// pdfInflater reuses decompressor state across the many streams of a PDF.
type pdfInflater struct {
	zlib io.ReadCloser
	raw  io.ReadCloser
}

func newPDFInflater() *pdfInflater {
	return &pdfInflater{raw: flate.NewReader(bytes.NewReader(nil))}
}

// inflate writes at most limit inflated bytes to out and returns the count.
// zlib-wrapped data is preferred; raw deflate is the fallback only for a
// stream whose dictionary declares FlateDecode.
func (p *pdfInflater) inflate(out *bytes.Buffer, body *chargedReader, allowRaw bool, limit int64) int64 {
	var reader io.Reader
	switch {
	case hasZlibHeader(body.data):
		if err := p.resetZlib(body); err != nil {
			return 0
		}
		reader = p.zlib
	case allowRaw:
		if err := p.raw.(flate.Resetter).Reset(body, nil); err != nil {
			return 0
		}
		reader = p.raw
	default:
		return 0
	}
	n, _ := io.Copy(out, io.LimitReader(reader, limit))
	return n
}

func (p *pdfInflater) resetZlib(body io.Reader) error {
	if p.zlib == nil {
		r, err := zlib.NewReader(body)
		if err != nil {
			return err
		}
		p.zlib = r
		return nil
	}
	return p.zlib.(zlib.Resetter).Reset(body, nil)
}

func isPDFWhitespace(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func isPDFDelimiter(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return isPDFWhitespace(c)
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
