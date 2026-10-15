package admission

import (
	"bytes"
	"compress/lzw"
	"encoding/ascii85"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

const pdfActionObjects = "7 0 << /S /JavaScript /JS (app.alert(1)) >>"

// pdfObjectStream wraps encoded bytes in an object stream with the given
// filter and decode parameters (raw PDF syntax, may be empty).
func pdfObjectStream(filter, parms string, encoded []byte) string {
	return fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Filter %s %s /Length %d >>\nstream\n%s\nendstream",
		filter, parms, len(encoded), encoded)
}

func hexEncoded(s string) []byte { return []byte(fmt.Sprintf("%X>", s)) }

func ascii85Encoded(s string) []byte {
	out := make([]byte, ascii85.MaxEncodedLen(len(s)))
	n := ascii85.Encode(out, []byte(s))
	return append(out[:n], "~>"...)
}

// lzwEncoded uses Go's LZW, which matches PDF LZW with /EarlyChange 0.
func lzwEncoded(t *testing.T, s string) []byte {
	var out bytes.Buffer
	w := lzw.NewWriter(&out, lzw.MSB, 8)
	_, err := w.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

func runLengthEncoded(s string) []byte {
	var out []byte
	for len(s) > 0 {
		n := min(len(s), 128)
		out = append(out, byte(n-1))
		out = append(out, s[:n]...)
		s = s[n:]
	}
	return append(out, 128)
}

// pngUpPredicted encodes s as rows of width bytes with the PNG Up filter, so
// a name spanning rows never appears in the inflated bytes.
func pngUpPredicted(s string, width int) []byte {
	data := []byte(s)
	for len(data)%width != 0 {
		data = append(data, ' ')
	}
	var out []byte
	prev := make([]byte, width)
	for start := 0; start < len(data); start += width {
		out = append(out, 2)
		for i, c := range data[start : start+width] {
			out = append(out, c-prev[i])
		}
		copy(prev, data[start:start+width])
	}
	return out
}

func TestEvaluatePDFDecodesEveryFilterBeforeClearing(t *testing.T) {
	predicted := pngUpPredicted(pdfActionObjects, 4)
	require.NotContains(t, string(predicted), "JavaScript")
	for _, tc := range []struct {
		name   string
		stream string
		reason string
	}{
		{name: "ascii hex object stream (review case)", stream: pdfObjectStream("/ASCIIHexDecode", "", hexEncoded(pdfActionObjects)), reason: ReasonActiveContent},
		{name: "abbreviated ascii hex", stream: pdfObjectStream("/AHx", "", hexEncoded(pdfActionObjects)), reason: ReasonActiveContent},
		{name: "ascii85 object stream", stream: pdfObjectStream("/ASCII85Decode", "", ascii85Encoded(pdfActionObjects)), reason: ReasonActiveContent},
		{name: "hex then flate chain", stream: pdfObjectStream("[/ASCIIHexDecode /FlateDecode]", "", hexEncoded(string(zlibBytes(t, []byte(pdfActionObjects))))), reason: ReasonActiveContent},
		{name: "ascii85 then flate chain with null parms", stream: pdfObjectStream("[/A85 /Fl]", "/DecodeParms [null null]", ascii85Encoded(string(zlibBytes(t, []byte(pdfActionObjects))))), reason: ReasonActiveContent},
		{name: "lzw object stream", stream: pdfObjectStream("/LZWDecode", "/DecodeParms << /EarlyChange 0 >>", lzwEncoded(t, pdfActionObjects)), reason: ReasonActiveContent},
		{name: "run length object stream", stream: pdfObjectStream("/RunLengthDecode", "", runLengthEncoded(pdfActionObjects)), reason: ReasonActiveContent},
		{name: "flate with png predictor", stream: pdfObjectStream("/FlateDecode", "/DecodeParms << /Predictor 12 /Columns 4 >>", zlibBytes(t, predicted)), reason: ReasonActiveContent},
		{name: "unknown filter object stream", stream: pdfObjectStream("/FooDecode", "", []byte("opaque")), reason: ReasonMalformed},
		{name: "raster codec on object stream", stream: pdfObjectStream("/DCTDecode", "", []byte("opaque")), reason: ReasonMalformed},
		{name: "crypt filter", stream: pdfObjectStream("/Crypt", "", []byte("opaque")), reason: ReasonMalformed},
		{name: "indirect filter", stream: pdfObjectStream("8 0 R", "", []byte("opaque")), reason: ReasonMalformed},
		{name: "invalid ascii85", stream: pdfObjectStream("/ASCII85Decode", "", []byte("abc{~>")), reason: ReasonMalformed},
		{name: "unsupported tiff predictor depth", stream: pdfObjectStream("/FlateDecode", "/DecodeParms << /Predictor 2 /BitsPerComponent 4 >>", zlibBytes(t, []byte("x"))), reason: ReasonMalformed},
		{name: "benign hex content", stream: fmt.Sprintf("<< /Filter /ASCIIHexDecode /Length 9 >>\nstream\n%s\nendstream", hexEncoded("0 0 m")), reason: ""},
		{name: "benign lzw with default early change", stream: "<< /Filter /LZWDecode >>\nstream\n\x80\x0b\x60\x50\x22\x0c\x0c\x85\x01\nendstream", reason: ""},
		{name: "image stream left to its raster codec", stream: "<< /Type /XObject /Subtype /Image /Filter /DCTDecode /Length 6 >>\nstream\nopaque\nendstream", reason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Evaluate(named(admissiontest.PDFWith(tc.stream), "a.pdf"))
			if tc.reason == "" {
				assert.True(t, result.Accepted, result.Reason)
				return
			}
			assert.False(t, result.Accepted)
			assert.Equal(t, tc.reason, result.Reason)
		})
	}
}

// Section 6: "Header/xref parse succeeds". Documents that only carry the
// lexical markers are not PDFs a reader can open.
func TestEvaluatePDFRequiresDocumentStructure(t *testing.T) {
	bare := "%PDF-1.7\nstartxref\n0\n%%EOF\n"
	noObjects := "%PDF-1.7\nxref\n0 1\n0000000000 65535 f \ntrailer\n<< /Size 1 /Root 1 0 R >>\nstartxref\n9\n%%EOF\n"
	noRoot := strings.Replace(string(admissiontest.PDF()), "/Root 1 0 R", "/Info 1 0 R", 1)
	noPages := strings.ReplaceAll(strings.ReplaceAll(string(admissiontest.PDF()), "/Pages 2 0 R", "/Outlines 2 0 R"), "/Type /Pages", "/Type /Node")
	noXRef := strings.Replace(string(admissiontest.PDF()), "xref\n0 ", "XREF\n0 ", 1)
	for name, data := range map[string]string{
		"bare markers": bare, "no objects": noObjects, "no root": noRoot, "no page tree": noPages, "no xref": noXRef,
	} {
		t.Run(name, func(t *testing.T) {
			result := Evaluate(named([]byte(data), "a.pdf"))
			assert.False(t, result.Accepted)
			assert.Equal(t, ReasonMalformed, result.Reason)
		})
	}
}

// A PDF 1.5 cross-reference stream replaces the xref table.
func TestEvaluatePDFAcceptsCrossReferenceStream(t *testing.T) {
	doc := string(admissiontest.PDF())
	body := doc[:strings.Index(doc, "xref\n")]
	xref := fmt.Sprintf("6 0 obj\n<< /Type /XRef /Size 7 /Root 1 0 R /W [1 2 1] /Length 4 >>\nstream\n\x01\x00\x0f\x00\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", len(body))
	result := Evaluate(named([]byte(body+xref), "a.pdf"))
	assert.True(t, result.Accepted, result.Reason)
}

func TestLZWDecodeMatchesEarlyChangeVariants(t *testing.T) {
	text := strings.Repeat("TOBEORNOTTOBEORTOBEORNOT#", 400)
	var out bytes.Buffer
	require.NoError(t, lzwDecode(&chargedReader{data: lzwEncoded(t, text), b: newBudget()}, &out, 1<<20, 0))
	assert.Equal(t, text, out.String())

	// The PDF reference's EarlyChange 1 example: "-----A---B".
	out.Reset()
	require.NoError(t, lzwDecode(&chargedReader{data: []byte("\x80\x0b\x60\x50\x22\x0c\x0c\x85\x01"), b: newBudget()}, &out, 1<<20, 1))
	assert.Equal(t, "-----A---B", out.String())
}
