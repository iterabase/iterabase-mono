package admission

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"regexp"
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

// Structural markers recorded alongside reasons in the scan's found set.
const (
	pdfSawPages      = "structure:pages"
	pdfSawXRefStream = "structure:xref-stream"
	// pdfSawUndeclaredStream marks a stream whose dictionary could not be
	// located, so its filter chain is unknown.
	pdfSawUndeclaredStream = "structure:undeclared-stream"
)

var (
	pdfIndirectObject = regexp.MustCompile(`(?:^|[^0-9])[0-9]{1,10}[ \t\r\n\f\x00]+[0-9]{1,5}[ \t\r\n\f\x00]+obj[ \t\r\n\f\x00<\[(/]`)
	pdfRootReference  = regexp.MustCompile(`/Root[ \t\r\n\f\x00]*[0-9]{1,10}[ \t\r\n\f\x00]+[0-9]{1,5}[ \t\r\n\f\x00]+R`)
	pdfStartXRef      = regexp.MustCompile(`startxref[ \t\r\n\f\x00]+[0-9]{1,20}[ \t\r\n\f\x00]`)
	pdfXRefTable      = regexp.MustCompile(`(?:^|[\r\n])xref[ \t]*\r?\n[ \t]*[0-9]+[ \t]+[0-9]+`)
)

func validatePDF(data []byte) string {
	if !bytes.Contains(head(data, 1024), []byte("%PDF-")) ||
		!bytes.Contains(data[max(0, len(data)-1024):], []byte("%%EOF")) ||
		!pdfStartXRef.Match(data) {
		return ReasonMalformed
	}
	found := make(map[string]bool)
	scanPDFNames(data, found)
	// An encrypted document's streams are ciphertext; stop before decoding.
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
	if found[pdfSawUndeclaredStream] {
		return ReasonMalformed
	}
	// Header and xref must parse (section 6): a cross-reference table or
	// stream, at least one indirect object, a trailer /Root reference, and a
	// page tree. Every check runs on bytes the scan has already examined.
	hasXRef := pdfXRefTable.Match(data) || found[pdfSawXRefStream]
	if !hasXRef || !pdfIndirectObject.Match(data) || !pdfRootReference.Match(data) || !found[pdfSawPages] {
		return ReasonMalformed
	}
	return ""
}

func scanPDFNames(data []byte, found map[string]bool) {
	forEachPDFName(data, func(name []byte) {
		if reason, ok := pdfNameReasons[string(name)]; ok {
			found[reason] = true
		}
		if string(name) == "Pages" {
			found[pdfSawPages] = true
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

// scanPDFStreams decodes every filtered stream and scans the decoded bytes
// for forbidden names. Hostile-input hardening:
//   - Decoding starts at the stream body and runs to each filter's own
//     end-of-data, not to a possibly forged "endstream" or /Length.
//   - A body with a valid zlib header is inflated even when its dictionary
//     cannot be found, so a malformed dictionary cannot hide a stream.
//   - A stream whose filter chain cannot be determined or decoded is rejected
//     rather than skipped, unless its chain ends in a raster codec and it is
//     not an object stream (raster samples cannot hold PDF objects).
//   - Inflate errors are tolerated and the partial output is scanned, because
//     a lenient reader may still render a truncated stream.
//   - Consumed and produced bytes are charged to one budget, so overlapping
//     or empty-block streams cannot go quadratic.
func scanPDFStreams(data []byte, found map[string]bool) string {
	b := newBudget()
	inflater := newPDFInflater()
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
		if !ok || (keyword > 0 && !isPDFWhitespace(data[keyword-1]) && !isPDFDelimiter(data[keyword-1])) {
			continue // not the stream keyword token
		}
		rawDict, dictFound := streamDict(data, keyword, floor)
		if !dictFound {
			// A stream whose dictionary cannot be located cannot have its
			// filter chain determined, so the document is rejected rather
			// than the stream skipped. Its body is still sniffed below, so a
			// more specific reason such as active content takes precedence.
			found[pdfSawUndeclaredStream] = true
		}
		floor = start
		if end := bytes.Index(data[start:], []byte("endstream")); end >= 0 {
			pos = start + end + len("endstream")
		} else {
			pos = len(data)
		}
		chain, objectStream, reason := streamFilterChain(data[start:], rawDict, dictFound, found)
		if reason != "" {
			return reason
		}
		if len(chain) == 0 {
			continue
		}
		decoded, status := decodePDFStream(data[start:], chain, objectStream, b, inflater)
		switch status {
		case pdfOverBudget:
			return ReasonResourceLimit
		case pdfUndecodable:
			return ReasonMalformed
		case pdfRasterOnly:
			continue
		}
		scanPDFNames(decoded, found)
	}
	return ""
}

// streamFilterChain returns the chain to decode a stream body with, and
// whether it is an object stream. An empty chain means the body needs no
// decoding: unfiltered bytes were covered by the whole-file name scan. A body
// with a zlib header is inflated even without a usable declaration, in case a
// reader sniffs it.
func streamFilterChain(body, rawDict []byte, dictFound bool, found map[string]bool) ([]pdfFilter, bool, string) {
	var chain []pdfFilter
	objectStream := false
	if dictFound {
		dict, parsed := parsePDFDict(rawDict)
		if !parsed {
			return nil, false, ReasonMalformed
		}
		if isPDFNameValue(dict["Type"], "XRef") {
			found[pdfSawXRefStream] = true
		}
		objectStream = isPDFNameValue(dict["Type"], "ObjStm")
		var ok bool
		if chain, ok = pdfFilterChain(dict); !ok {
			return nil, false, ReasonMalformed
		}
	}
	if len(chain) == 0 && hasZlibHeader(body) {
		chain = []pdfFilter{{name: "FlateDecode"}}
	}
	return chain, objectStream, ""
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

// streamDict returns the dictionary that precedes a stream keyword,
// separated from it only by whitespace and comments, matching nested << >>
// within a bounded look-behind above floor.
func streamDict(data []byte, keyword, floor int) ([]byte, bool) {
	end := skipTrailingPDFTrivia(data, keyword, floor)
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

// skipTrailingPDFTrivia moves end back over whitespace and comments. A
// comment runs from a % to the end of its line; scanning backwards, the
// rightmost % on a line that leaves the line ending in ">>" is taken as the
// comment start, so a % inside a string literal earlier on the line is not.
// Anything ambiguous leaves end where a dictionary cannot be matched, and the
// stream is then rejected.
func skipTrailingPDFTrivia(data []byte, end, floor int) int {
	for {
		for end > floor && isPDFWhitespace(data[end-1]) {
			end--
		}
		if endsWithDictClose(data, floor, end) {
			return end
		}
		lineStart := end
		for lineStart > floor && data[lineStart-1] != '\n' && data[lineStart-1] != '\r' {
			lineStart--
		}
		comment := pdfCommentStart(data, lineStart, end)
		if comment < 0 {
			return end
		}
		end = comment
	}
}

// pdfCommentStart returns the rightmost % in data[lineStart:end] that has only
// blanks between it and either the line start or a ">>", or -1. Each % skips
// back over only the blanks after the previous token, so it runs in linear
// time.
func pdfCommentStart(data []byte, lineStart, end int) int {
	for i := end - 1; i >= lineStart; i-- {
		if data[i] != '%' {
			continue
		}
		j := i
		for j > lineStart && isPDFBlank(data[j-1]) {
			j--
		}
		if j == lineStart || endsWithDictClose(data, lineStart, j) {
			return i
		}
	}
	return -1
}

func endsWithDictClose(data []byte, floor, end int) bool {
	return end-2 >= floor && data[end-2] == '>' && data[end-1] == '>'
}

func isPDFBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\f' || c == 0 }

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
