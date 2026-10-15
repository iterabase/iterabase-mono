package admission

import (
	"bytes"
	"strconv"
)

// pdfKind is the kind of a parsed PDF object.
type pdfKind int

const (
	pdfOther pdfKind = iota
	pdfNull
	pdfName
	pdfNumber
	pdfRef
	pdfArray
	pdfDict
)

// pdfValue is one direct object of a parsed stream dictionary. Only the kinds
// the filter chain needs are modelled; strings and keywords are pdfOther.
type pdfValue struct {
	kind  pdfKind
	name  string
	num   int64
	isInt bool
	items []pdfValue
	dict  map[string]pdfValue
}

const pdfMaxSyntaxDepth = 32

// parsePDFDict parses one dictionary (`<< ... >>`) and reports whether it was
// well formed. It is a bounded recursive-descent parser over a slice the
// caller has already bounded, so it cannot read outside the dictionary.
func parsePDFDict(data []byte) (map[string]pdfValue, bool) {
	p := pdfParser{data: data}
	v, ok := p.value(0)
	if !ok || v.kind != pdfDict {
		return nil, false
	}
	p.skipSpace()
	return v.dict, p.pos == len(p.data)
}

type pdfParser struct {
	data []byte
	pos  int
}

func (p *pdfParser) skipSpace() {
	for p.pos < len(p.data) {
		switch c := p.data[p.pos]; {
		case isPDFWhitespace(c):
			p.pos++
		case c == '%':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *pdfParser) value(depth int) (pdfValue, bool) {
	if depth > pdfMaxSyntaxDepth {
		return pdfValue{}, false
	}
	p.skipSpace()
	if p.pos >= len(p.data) {
		return pdfValue{}, false
	}
	switch c := p.data[p.pos]; {
	case c == '/':
		return pdfValue{kind: pdfName, name: p.name()}, true
	case c == '<':
		return p.angle(depth)
	case c == '[':
		return p.array(depth)
	case c == '(':
		return pdfValue{kind: pdfOther}, p.literalString()
	case c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9'):
		return p.numberOrRef(), true
	case isPDFDelimiter(c):
		return pdfValue{}, false
	}
	token := p.token()
	if token == "null" {
		return pdfValue{kind: pdfNull}, true
	}
	return pdfValue{kind: pdfOther, name: token}, true
}

// angle parses a dictionary or a hex string.
func (p *pdfParser) angle(depth int) (pdfValue, bool) {
	if p.pos+1 < len(p.data) && p.data[p.pos+1] == '<' {
		return p.dictionary(depth)
	}
	end := bytes.IndexByte(p.data[p.pos:], '>')
	if end < 0 {
		return pdfValue{}, false
	}
	p.pos += end + 1
	return pdfValue{kind: pdfOther}, true
}

func (p *pdfParser) dictionary(depth int) (pdfValue, bool) {
	p.pos += 2
	dict := map[string]pdfValue{}
	for {
		p.skipSpace()
		if p.pos+1 < len(p.data) && p.data[p.pos] == '>' && p.data[p.pos+1] == '>' {
			p.pos += 2
			return pdfValue{kind: pdfDict, dict: dict}, true
		}
		if p.pos >= len(p.data) || p.data[p.pos] != '/' {
			return pdfValue{}, false
		}
		key := p.name()
		v, ok := p.value(depth + 1)
		if !ok {
			return pdfValue{}, false
		}
		dict[key] = v
	}
}

func (p *pdfParser) array(depth int) (pdfValue, bool) {
	p.pos++
	var items []pdfValue
	for {
		p.skipSpace()
		if p.pos < len(p.data) && p.data[p.pos] == ']' {
			p.pos++
			return pdfValue{kind: pdfArray, items: items}, true
		}
		v, ok := p.value(depth + 1)
		if !ok {
			return pdfValue{}, false
		}
		items = append(items, v)
	}
}

// name consumes a name token at p.pos (which is '/') and decodes #xx escapes.
func (p *pdfParser) name() string {
	p.pos++
	var out []byte
	for p.pos < len(p.data) && !isPDFDelimiter(p.data[p.pos]) {
		c := p.data[p.pos]
		if c == '#' && p.pos+2 < len(p.data) && isHex(p.data[p.pos+1]) && isHex(p.data[p.pos+2]) {
			c = unhex(p.data[p.pos+1])<<4 | unhex(p.data[p.pos+2])
			p.pos += 2
		}
		out = append(out, c)
		p.pos++
	}
	return string(out)
}

func (p *pdfParser) token() string {
	start := p.pos
	for p.pos < len(p.data) && !isPDFDelimiter(p.data[p.pos]) {
		p.pos++
	}
	return string(p.data[start:p.pos])
}

func (p *pdfParser) literalString() bool {
	depth := 0
	for ; p.pos < len(p.data); p.pos++ {
		switch p.data[p.pos] {
		case '\\':
			p.pos++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				p.pos++
				return true
			}
		}
	}
	return false
}

// numberOrRef parses a number, or an indirect reference `N G R`.
func (p *pdfParser) numberOrRef() pdfValue {
	first := p.token()
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return pdfValue{kind: pdfNumber}
	}
	save := p.pos
	p.skipSpace()
	if second := p.token(); second != "" {
		if _, err := strconv.ParseInt(second, 10, 64); err == nil {
			p.skipSpace()
			if p.token() == "R" {
				return pdfValue{kind: pdfRef}
			}
		}
	}
	p.pos = save
	return pdfValue{kind: pdfNumber, num: n, isInt: true}
}

func isPDFNameValue(v pdfValue, names ...string) bool {
	if v.kind != pdfName {
		return false
	}
	for _, name := range names {
		if v.name == name {
			return true
		}
	}
	return false
}
