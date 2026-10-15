package admission

import (
	"bytes"
	"errors"
	"io"
)

// pdfFilter is one stage of a stream's /Filter chain with its /DecodeParms.
type pdfFilter struct {
	name  string
	parms map[string]pdfValue
}

// Canonical filter names; the abbreviations are the inline-image forms that
// lenient readers also accept on streams.
var pdfFilterNames = map[string]string{
	"FlateDecode": "FlateDecode", "Fl": "FlateDecode",
	"LZWDecode": "LZWDecode", "LZW": "LZWDecode",
	"ASCIIHexDecode": "ASCIIHexDecode", "AHx": "ASCIIHexDecode",
	"ASCII85Decode": "ASCII85Decode", "A85": "ASCII85Decode",
	"RunLengthDecode": "RunLengthDecode", "RL": "RunLengthDecode",
	"DCTDecode": "DCTDecode", "DCT": "DCTDecode",
	"CCITTFaxDecode": "CCITTFaxDecode", "CCF": "CCITTFaxDecode",
	"JPXDecode":   "JPXDecode",
	"JBIG2Decode": "JBIG2Decode",
	"Crypt":       "Crypt",
}

// pdfImageCodecs produce raster samples only. Their output cannot hold PDF
// objects, so a non-object stream ending in one needs no inspection.
var pdfImageCodecs = map[string]bool{
	"DCTDecode": true, "CCITTFaxDecode": true, "JPXDecode": true, "JBIG2Decode": true,
}

// pdfFilterChain returns the declared filter chain. ok is false when the
// chain cannot be determined from the dictionary itself (for example an
// indirect /Filter), which the caller rejects rather than skipping.
func pdfFilterChain(dict map[string]pdfValue) ([]pdfFilter, bool) {
	var names []pdfValue
	switch filter, present := dict["Filter"]; {
	case !present || filter.kind == pdfNull:
		return nil, true
	case filter.kind == pdfName:
		names = []pdfValue{filter}
	case filter.kind == pdfArray:
		names = filter.items
	default:
		return nil, false
	}
	var parms []pdfValue
	switch decode, present := dict["DecodeParms"]; {
	case !present || decode.kind == pdfNull:
	case decode.kind == pdfDict:
		parms = []pdfValue{decode}
	case decode.kind == pdfArray:
		parms = decode.items
	default:
		return nil, false
	}
	chain := make([]pdfFilter, 0, len(names))
	for i, name := range names {
		canonical, known := pdfFilterNames[name.name]
		if name.kind != pdfName || !known {
			return nil, false
		}
		f := pdfFilter{name: canonical}
		if i < len(parms) {
			switch parms[i].kind {
			case pdfDict:
				f.parms = parms[i].dict
			case pdfNull:
			default:
				return nil, false
			}
		}
		chain = append(chain, f)
	}
	return chain, true
}

type pdfDecodeStatus int

const (
	pdfDecoded pdfDecodeStatus = iota
	// pdfRasterOnly: the chain ends in an image codec; nothing to inspect.
	pdfRasterOnly
	// pdfUndecodable: the bytes a strict reader would see cannot be derived.
	pdfUndecodable
	pdfOverBudget
)

var errPDFDecode = errors.New("pdf: undecodable stream")

// decodePDFStream applies chain to the stream body starting at body[0]. Each
// stage reads to its own end-of-data marker, not to a possibly forged
// "endstream", and every consumed and produced byte is charged to b.
func decodePDFStream(body []byte, chain []pdfFilter, objectStream bool, b *budget, inflater *pdfInflater) ([]byte, pdfDecodeStatus) {
	input := body
	for _, f := range chain {
		if pdfImageCodecs[f.name] {
			if objectStream {
				return nil, pdfUndecodable
			}
			return nil, pdfRasterOnly
		}
		decoded, status := decodePDFStage(f, input, b, inflater)
		if status != pdfDecoded {
			return nil, status
		}
		input = decoded
	}
	return input, pdfDecoded
}

// decodePDFStage applies one non-raster filter and its predictor.
func decodePDFStage(f pdfFilter, input []byte, b *budget, inflater *pdfInflater) ([]byte, pdfDecodeStatus) {
	reader := &chargedReader{data: input, b: b}
	var out bytes.Buffer
	err := runPDFDecoder(f, reader, &out, min(int64(MaxPDFStreamBytes), b.remaining)+1, inflater)
	n := int64(out.Len())
	if reader.exhausted || n > MaxPDFStreamBytes || !b.take(n) {
		return nil, pdfOverBudget
	}
	if err != nil {
		return nil, pdfUndecodable
	}
	decoded := out.Bytes()
	if f.name == "FlateDecode" || f.name == "LZWDecode" {
		if decoded, err = unpredict(decoded, f.parms); err != nil {
			return nil, pdfUndecodable
		}
	}
	return decoded, pdfDecoded
}

func runPDFDecoder(f pdfFilter, reader *chargedReader, out *bytes.Buffer, limit int64, inflater *pdfInflater) error {
	switch f.name {
	case "FlateDecode":
		inflater.inflate(out, reader, true, limit)
		return nil
	case "LZWDecode":
		early, ok := pdfIntParm(f.parms, "EarlyChange", 1)
		if !ok || (early != 0 && early != 1) {
			return errPDFDecode
		}
		return lzwDecode(reader, out, limit, int(early))
	case "ASCIIHexDecode":
		asciiHexDecode(reader, out, limit)
		return nil
	case "ASCII85Decode":
		return ascii85Decode(reader, out, limit)
	case "RunLengthDecode":
		runLengthDecode(reader, out, limit)
		return nil
	}
	return errPDFDecode
}

func pdfIntParm(parms map[string]pdfValue, key string, fallback int64) (int64, bool) {
	v, present := parms[key]
	if !present || v.kind == pdfNull {
		return fallback, true
	}
	return v.num, v.kind == pdfNumber && v.isInt
}

// unpredict reverses a TIFF (2) or PNG (10-15) predictor so names split
// across predicted rows are visible again.
func unpredict(data []byte, parms map[string]pdfValue) ([]byte, error) {
	predictor, ok := pdfIntParm(parms, "Predictor", 1)
	if !ok {
		return nil, errPDFDecode
	}
	if predictor == 1 {
		return data, nil
	}
	bits, pixel, row, err := predictorGeometry(parms)
	if err != nil {
		return nil, err
	}
	switch {
	case predictor == 2:
		if bits != 8 {
			return nil, errPDFDecode
		}
		out := bytes.Clone(data)
		for start := 0; start < len(out); start += row {
			line := out[start:min(start+row, len(out))]
			for i := pixel; i < len(line); i++ {
				line[i] += line[i-pixel]
			}
		}
		return out, nil
	case predictor >= 10 && predictor <= 15:
		return unpredictPNG(data, row, pixel)
	}
	return nil, errPDFDecode
}

// predictorGeometry returns the component depth and the pixel and row widths
// in bytes of predicted data.
func predictorGeometry(parms map[string]pdfValue) (bits int64, pixel, row int, err error) {
	colors, ok1 := pdfIntParm(parms, "Colors", 1)
	bits, ok2 := pdfIntParm(parms, "BitsPerComponent", 8)
	columns, ok3 := pdfIntParm(parms, "Columns", 1)
	validBits := bits == 1 || bits == 2 || bits == 4 || bits == 8 || bits == 16
	if !ok1 || !ok2 || !ok3 || !validBits || colors < 1 || colors > 32 || columns < 1 || columns > 1<<20 {
		return 0, 0, 0, errPDFDecode
	}
	return bits, int((colors*bits + 7) / 8), int((colors*bits*columns + 7) / 8), nil
}

func unpredictPNG(data []byte, row, pixel int) ([]byte, error) {
	out := make([]byte, 0, len(data))
	prev := make([]byte, row)
	line := make([]byte, row)
	for start := 0; start < len(data); start += row + 1 {
		kind := data[start]
		n := copy(line, data[start+1:min(start+1+row, len(data))])
		for i := 0; i < n; i++ {
			var left, upLeft byte
			if i >= pixel {
				left, upLeft = line[i-pixel], prev[i-pixel]
			}
			switch kind {
			case 0:
			case 1:
				line[i] += left
			case 2:
				line[i] += prev[i]
			case 3:
				line[i] += left/2 + prev[i]/2 + left&prev[i]&1
			case 4:
				line[i] += paeth(left, prev[i], upLeft)
			default:
				return nil, errPDFDecode
			}
		}
		out = append(out, line[:n]...)
		copy(prev, line[:n])
	}
	return out, nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// asciiHexDecode decodes to '>' like common readers: whitespace and other
// non-hex bytes are skipped and an odd final digit is padded with zero.
func asciiHexDecode(r io.ByteReader, out *bytes.Buffer, limit int64) {
	high, pending := byte(0), false
	for int64(out.Len()) < limit {
		c, err := r.ReadByte()
		if err != nil || c == '>' {
			break
		}
		if !isHex(c) {
			continue
		}
		if pending {
			out.WriteByte(high<<4 | unhex(c))
		} else {
			high = unhex(c)
		}
		pending = !pending
	}
	if pending && int64(out.Len()) < limit {
		out.WriteByte(high << 4)
	}
}

// ascii85Decode decodes to '~>'. Bytes outside the alphabet fail closed.
func ascii85Decode(r io.ByteReader, out *bytes.Buffer, limit int64) error {
	d := ascii85Decoder{out: out}
	for int64(out.Len()) < limit {
		c, err := r.ReadByte()
		if err != nil {
			break
		}
		if c == '~' {
			if next, err := r.ReadByte(); err != nil || next != '>' {
				return errPDFDecode
			}
			break
		}
		if err := d.feed(c); err != nil {
			return err
		}
	}
	return d.finish()
}

type ascii85Decoder struct {
	out   *bytes.Buffer
	group [5]byte
	n     int
}

func (d *ascii85Decoder) feed(c byte) error {
	switch {
	case isPDFWhitespace(c):
		return nil
	case c == 'z' && d.n == 0:
		d.out.Write([]byte{0, 0, 0, 0})
		return nil
	case c < '!' || c > 'u':
		return errPDFDecode
	}
	d.group[d.n] = c
	if d.n++; d.n < 5 {
		return nil
	}
	d.n = 0
	return d.flush(4)
}

// finish decodes a final partial group, padded with 'u'.
func (d *ascii85Decoder) finish() error {
	switch d.n {
	case 0:
		return nil
	case 1:
		return errPDFDecode
	}
	count := d.n - 1
	for i := d.n; i < 5; i++ {
		d.group[i] = 'u'
	}
	return d.flush(count)
}

func (d *ascii85Decoder) flush(count int) error {
	var value uint64
	for _, c := range d.group {
		value = value*85 + uint64(c-'!')
	}
	if value > 0xFFFFFFFF {
		return errPDFDecode
	}
	word := [4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
	d.out.Write(word[:count])
	return nil
}

// runLengthDecode decodes to the 128 end-of-data length byte.
func runLengthDecode(r io.ByteReader, out *bytes.Buffer, limit int64) {
	for int64(out.Len()) < limit {
		length, err := r.ReadByte()
		if err != nil || length == 128 {
			return
		}
		if length < 128 {
			for i := 0; i <= int(length); i++ {
				c, err := r.ReadByte()
				if err != nil {
					return
				}
				out.WriteByte(c)
			}
			continue
		}
		c, err := r.ReadByte()
		if err != nil {
			return
		}
		for i := 0; i < 257-int(length); i++ {
			out.WriteByte(c)
		}
	}
}

// lzwDecode decodes PDF LZW (MSB-first, 9-12 bit codes, 256 clear, 257 end)
// with the given EarlyChange. An invalid code fails closed.
func lzwDecode(r io.ByteReader, out *bytes.Buffer, limit int64, early int) error {
	table := newLZWTable()
	var bits uint32
	nbits, width := 0, 9
	for int64(out.Len()) < limit {
		for nbits < width {
			c, err := r.ReadByte()
			if err != nil {
				return nil
			}
			bits = bits<<8 | uint32(c)
			nbits += 8
		}
		code := int(bits>>(nbits-width)) & (1<<width - 1)
		nbits -= width
		switch code {
		case lzwClear:
			table.reset()
			width = 9
			continue
		case lzwEnd:
			return nil
		}
		if err := table.decode(code, out); err != nil {
			return err
		}
		for width < 12 && table.next+early >= 1<<width {
			width++
		}
	}
	return nil
}

const (
	lzwTableSize = 4096
	lzwClear     = 256
	lzwEnd       = 257
)

// lzwTable is the LZW string table as prefix links, so emitting a code costs
// its length and the table is fixed-size.
type lzwTable struct {
	prefix  []int
	suffix  []byte
	first   []byte
	length  []int
	scratch []byte
	next    int
	prev    int
}

func newLZWTable() *lzwTable {
	t := &lzwTable{
		prefix: make([]int, lzwTableSize), suffix: make([]byte, lzwTableSize), first: make([]byte, lzwTableSize),
		length: make([]int, lzwTableSize), scratch: make([]byte, lzwTableSize),
	}
	for i := range 256 {
		t.suffix[i], t.first[i], t.length[i] = byte(i), byte(i), 1
	}
	t.reset()
	return t
}

func (t *lzwTable) reset() { t.next, t.prev = lzwEnd+1, -1 }

// decode emits code and records the new table entry.
func (t *lzwTable) decode(code int, out *bytes.Buffer) error {
	if t.prev < 0 {
		if code > 255 {
			return errPDFDecode
		}
		t.emit(code, out)
		t.prev = code
		return nil
	}
	var head byte
	switch {
	case code < t.next:
		t.emit(code, out)
		head = t.first[code]
	case code == t.next:
		t.emit(t.prev, out)
		head = t.first[t.prev]
		out.WriteByte(head)
	default:
		return errPDFDecode
	}
	if t.next < lzwTableSize {
		t.prefix[t.next], t.suffix[t.next], t.first[t.next] = t.prev, head, t.first[t.prev]
		t.length[t.next] = t.length[t.prev] + 1
		t.next++
	}
	t.prev = code
	return nil
}

func (t *lzwTable) emit(code int, out *bytes.Buffer) {
	n := t.length[code]
	for i := n - 1; i >= 0; i-- {
		t.scratch[i] = t.suffix[code]
		code = t.prefix[code]
	}
	out.Write(t.scratch[:n])
}
