package admission

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"unicode/utf8"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// markupOpenings mark text that a browser would sniff as HTML, SVG, or XML;
// such bytes are rejected as standalone artifacts whatever their name.
var markupOpenings = [][]byte{
	[]byte("<!doctype html"), []byte("<html"), []byte("<head"), []byte("<body"),
	[]byte("<script"), []byte("<iframe"), []byte("<svg"), []byte("<?xml"),
}

// validateText enforces the CSV/TXT policy: valid UTF-8 with an optional BOM,
// no NUL, no binary control profile, no markup, and for CSV a bounded parse.
func validateText(data []byte, isCSV bool) string {
	body := bytes.TrimPrefix(data, utf8BOM)
	if reason := textEncodingReason(body); reason != "" {
		return reason
	}
	if isMarkup(body) {
		return ReasonUnsupportedType
	}
	if isCSV {
		return validateCSV(body)
	}
	return ""
}

func textEncodingReason(body []byte) string {
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return ReasonInvalidTextEncoding
	}
	controls := 0
	for _, b := range body {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' {
			controls++
		}
	}
	// More than 1% C0 controls is a binary profile, not text.
	if controls*100 > len(body) {
		return ReasonInvalidTextEncoding
	}
	return ""
}

// looksTextual reports whether data would pass the text encoding policy.
func looksTextual(data []byte) bool {
	return textEncodingReason(bytes.TrimPrefix(data, utf8BOM)) == ""
}

func isMarkup(body []byte) bool {
	opening := bytes.ToLower(head(bytes.TrimLeft(body, " \t\r\n\f"), 32))
	for _, marker := range markupOpenings {
		if bytes.HasPrefix(opening, marker) {
			return true
		}
	}
	return false
}

func validateCSV(body []byte) string {
	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = true
	cells := 0
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ReasonMalformed
		}
		cells += len(record)
		if cells > MaxCSVCells {
			return ReasonResourceLimit
		}
		for _, field := range record {
			if len(field) > MaxCSVFieldBytes {
				return ReasonResourceLimit
			}
		}
	}
}
