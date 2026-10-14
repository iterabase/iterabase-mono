package admission

import (
	"bytes"
	"compress/flate"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

type evalCase struct {
	name   string
	in     Input
	mime   string // expected canonical type when accepted
	reason string // expected reason when rejected
}

func runEvalCases(t *testing.T, cases []evalCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.in)
			if tc.reason != "" {
				assert.False(t, got.Accepted)
				assert.Equal(t, tc.reason, got.Reason)
				assert.Empty(t, got.CanonicalMIME)
				assert.Empty(t, got.Children)
				return
			}
			assert.True(t, got.Accepted, "rejected with %q", got.Reason)
			assert.Equal(t, tc.mime, got.CanonicalMIME)
			assert.Empty(t, got.Reason)
		})
	}
}

func named(data []byte, filename string) Input {
	return Input{Data: data, Filename: filename, ContainerDepth: 1}
}

func claimed(data []byte, filename, claim string) Input {
	in := named(data, filename)
	in.ClaimedMIME = claim
	return in
}

func TestEvaluateAcceptsEveryAllowedType(t *testing.T) {
	runEvalCases(t, []evalCase{
		{name: "pdf", in: claimed(admissiontest.PDF(), "report.pdf", "application/pdf"), mime: MIMEPDF},
		{name: "pdf alias claim", in: claimed(admissiontest.PDF(), "report.pdf", "application/x-pdf"), mime: MIMEPDF},
		{name: "docx", in: claimed(admissiontest.DOCX(), "letter.docx", MIMEDOCX), mime: MIMEDOCX},
		{name: "xlsx", in: claimed(admissiontest.XLSX(), "book.xlsx", "application/octet-stream"), mime: MIMEXLSX},
		{name: "csv", in: claimed([]byte("a,b,c\n1,\"=SUM(A1)\",3\n"), "data.csv", "text/csv; charset=utf-8"), mime: MIMECSV},
		{name: "csv alias claim", in: claimed([]byte("a,b\n"), "data.csv", "application/csv"), mime: MIMECSV},
		{name: "txt with BOM", in: claimed(append(utf8BOM, "héllo\r\nworld\n"...), "notes.txt", "text/plain; charset=UTF-8"), mime: MIMETXT},
		{name: "generated txt without extension", in: Input{Data: []byte("pasted"), Generated: true}, mime: MIMETXT},
		{name: "jpeg", in: claimed(admissiontest.JPEG(), "photo.JPG", "image/jpg"), mime: MIMEJPEG},
		{name: "jpeg long extension", in: named(admissiontest.JPEG(), "photo.jpeg"), mime: MIMEJPEG},
		{name: "png", in: claimed(admissiontest.PNG(), "scan.png", "image/png"), mime: MIMEPNG},
		{name: "tiff", in: claimed(admissiontest.TIFF(), "scan.tiff", "image/tif"), mime: MIMETIFF},
		{name: "tiff short extension", in: named(admissiontest.TIFF(), "scan.tif"), mime: MIMETIFF},
		{name: "eml", in: claimed([]byte("Subject: hi\r\n\r\nbody\r\n"), "mail.eml", "message/rfc822"), mime: MIMEEML},
		{name: "msg", in: claimed(admissiontest.BuildMSG("Quarterly report", "body", nil), "mail.msg", "application/vnd.ms-outlook"), mime: MIMEMSG},
		{name: "msg x-msg claim", in: claimed(admissiontest.BuildMSG("Quarterly report", "body", nil), "mail.msg", "application/x-msg"), mime: MIMEMSG},
	})
}

func TestEvaluateRejectsTypeAndNameViolations(t *testing.T) {
	elf := append([]byte("\x7fELF\x02\x01\x01\x00"), make([]byte, 64)...)
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), make([]byte, 64)...)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	runEvalCases(t, []evalCase{
		{name: "empty", in: named(nil, "a.txt"), reason: ReasonEmptyFile},
		{name: "oversized", in: named(make([]byte, MaxFileBytes+1), "a.txt"), reason: ReasonResourceLimit},
		{name: "claim mismatch", in: claimed(admissiontest.PDF(), "a.pdf", "image/png"), reason: ReasonTypeMismatch},
		{name: "claim html on txt", in: claimed([]byte("hi"), "a.txt", "text/html"), reason: ReasonTypeMismatch},
		{name: "csv claim on txt", in: claimed([]byte("a,b"), "a.txt", "text/csv"), reason: ReasonTypeMismatch},
		{name: "extension mismatch pdf as png", in: named(admissiontest.PDF(), "a.png"), reason: ReasonTypeMismatch},
		{name: "extension mismatch png as jpg", in: named(admissiontest.PNG(), "a.jpg"), reason: ReasonTypeMismatch},
		{name: "docx named xlsx", in: named(admissiontest.DOCX(), "a.xlsx"), reason: ReasonTypeMismatch},
		{name: "text named pdf", in: named([]byte("not a pdf"), "a.pdf"), reason: ReasonTypeMismatch},
		{name: "missing extension not generated", in: named([]byte("x"), "README"), reason: ReasonUnsupportedType},
		{name: "missing extension generated pdf", in: Input{Data: admissiontest.PDF(), Generated: true}, reason: ReasonTypeMismatch},
		{name: "unknown extension", in: named([]byte("x"), "a.html"), reason: ReasonUnsupportedType},
		{name: "renamed exe", in: named(exe, "a.pdf"), reason: ReasonUnsupportedType},
		{name: "renamed elf", in: named(elf, "a.txt"), reason: ReasonUnsupportedType},
		{name: "shebang script", in: named([]byte("#!/bin/sh\nrm -rf /\n"), "a.txt"), reason: ReasonUnsupportedType},
		{name: "renamed gif", in: named(gif, "a.png"), reason: ReasonUnsupportedType},
		{name: "renamed generic zip", in: named(admissiontest.ZipOf(admissiontest.ZipEntry{Name: "a.txt", Data: []byte("x")}), "a.docx"), reason: ReasonUnsupportedType},
		{name: "renamed html", in: named([]byte("<!DOCTYPE html><html><script>x</script></html>"), "a.txt"), reason: ReasonUnsupportedType},
		{name: "renamed svg", in: named([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "a.csv"), reason: ReasonUnsupportedType},
		{name: "random binary named pdf", in: named([]byte{0x00, 0x01, 0x02, 0xFF}, "a.pdf"), reason: ReasonUnsupportedType},
		{name: "unsafe filename", in: named([]byte("x"), "invoice.pdf.exe"), reason: ReasonUnsafeFilename},
		{name: "empty filename not generated", in: named([]byte("x"), " . "), reason: ReasonUnsafeFilename},
		{name: "legacy cfb", in: named(admissiontest.CFB(admissiontest.CFBStream("WordDocument", []byte("doc"))), "a.msg"), reason: ReasonUnsupportedType},
		{name: "encrypted ooxml", in: named(admissiontest.EncryptedOOXML(), "a.docx"), reason: ReasonEncrypted},
	})
}

func TestEvaluateText(t *testing.T) {
	controls := append([]byte(strings.Repeat("a", 50)), 0x01, 0x02)
	runEvalCases(t, []evalCase{
		{name: "invalid utf8", in: named([]byte("caf\xe9"), "a.txt"), reason: ReasonInvalidTextEncoding},
		{name: "nul", in: named([]byte("a\x00b"), "a.txt"), reason: ReasonInvalidTextEncoding},
		{name: "binary control profile", in: named(controls, "a.txt"), reason: ReasonInvalidTextEncoding},
		{name: "csv bare quote is lazy", in: named([]byte("a,b\"c\n"), "a.csv"), mime: MIMECSV},
		{name: "csv field too large", in: named(bytes.Repeat([]byte("x"), MaxCSVFieldBytes+1), "a.csv"), reason: ReasonResourceLimit},
		{name: "csv too many cells", in: named([]byte(strings.Repeat(",", MaxCSVCells)), "a.csv"), reason: ReasonResourceLimit},
	})
}

func TestEvaluatePDF(t *testing.T) {
	runEvalCases(t, []evalCase{
		{name: "missing eof", in: named(bytes.TrimSuffix(admissiontest.PDF(), []byte("%%EOF\n")), "a.pdf"), reason: ReasonMalformed},
		{name: "encrypt", in: named(admissiontest.EncryptedPDF(), "a.pdf"), reason: ReasonEncrypted},
		{name: "javascript", in: named(admissiontest.ActivePDF(), "a.pdf"), reason: ReasonActiveContent},
		{name: "hex escaped javascript", in: named(admissiontest.PDFWith("<< /S /J#61vaScript >>"), "a.pdf"), reason: ReasonActiveContent},
		{name: "launch", in: named(admissiontest.PDFWith("<< /S /Launch /F (calc.exe) >>"), "a.pdf"), reason: ReasonActiveContent},
		{name: "additional actions", in: named(admissiontest.PDFWith("<< /AA << /O 5 0 R >> >>"), "a.pdf"), reason: ReasonActiveContent},
		{name: "embedded file", in: named(admissiontest.PDFWith("<< /Type /EmbeddedFile /Length 0 >>"), "a.pdf"), reason: ReasonEmbeddedObject},
		{name: "js inside flate object stream", in: named(admissiontest.PDFWith(flateStreamRaw([]byte(hiddenZlib(t, "5 0 "+strings.Repeat("0 0 m ", 64)+"<< /S /JS (x) >>")))), "a.pdf"), reason: ReasonActiveContent},
		{name: "js in zlib stream without dictionary", in: named(admissiontest.PDFWith("[ ]\nstream\n"+hiddenZlib(t, strings.Repeat("0 0 m ", 64)+"<< /S /JavaScript /JS (x) >>")+"\nendstream"), "a.pdf"), reason: ReasonActiveContent},
		{name: "js after forged endstream", in: named(admissiontest.PDFWith(forgedEndstreamObject(t)), "a.pdf"), reason: ReasonActiveContent},
		{name: "benign flate stream", in: named(admissiontest.PDFWith(flateStream(t, "5 0 << /Type /Font >>")), "a.pdf"), mime: MIMEPDF},
		{name: "flate stream bomb", in: named(admissiontest.PDFWith(flateStreamRaw(zlibZeroBomb(t, 65))), "a.pdf"), reason: ReasonResourceLimit},
	})
}

func TestEvaluateOOXML(t *testing.T) {
	external := `<Relationship Id="rId9" Type="` + admissiontest.OfficeRelationships + `/attachedTemplate" Target="http://evil.example/t.dotm" TargetMode="External"/>`
	hyperlink := `<Relationship Id="rId9" Type="` + admissiontest.OfficeRelationships + `/hyperlink" Target="https://example.com" TargetMode="External"/>`
	macroTypes := admissiontest.ZipOf(admissiontest.DOCXEntries("application/vnd.ms-word.document.macroEnabled.main+xml")...)
	manyEntries := make([]admissiontest.ZipEntry, 0, MaxPackageEntries+1)
	for i := range MaxPackageEntries + 1 {
		manyEntries = append(manyEntries, admissiontest.ZipEntry{Name: fmt.Sprintf("f%d", i)})
	}
	runEvalCases(t, []evalCase{
		{name: "vba project", in: named(admissiontest.MacroDOCX(), "a.docx"), reason: ReasonMacroContent},
		{name: "macro enabled content type", in: named(macroTypes, "a.docx"), reason: ReasonMacroContent},
		{name: "embeddings", in: named(admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/embeddings/oleObject1.bin", Data: []byte("ole")}), "a.docx"), reason: ReasonEmbeddedObject},
		{name: "activex", in: named(admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/activeX/activeX1.xml", Data: []byte("<a/>")}), "a.docx"), reason: ReasonEmbeddedObject},
		{name: "nested archive part", in: named(admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/media/x.bin", Data: admissiontest.ZipOf(admissiontest.ZipEntry{Name: "y", Data: nil})}), "a.docx"), reason: ReasonEmbeddedObject},
		{name: "external attached template", in: named(docxWithDocumentRels(external), "a.docx"), reason: ReasonExternalDependency},
		{name: "external hyperlink allowed", in: named(docxWithDocumentRels(hyperlink), "a.docx"), mime: MIMEDOCX},
		{name: "doctype", in: named(admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/settings.xml", Data: []byte(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "b">]><x/>`)}), "a.docx"), reason: ReasonMalformed},
		{name: "malformed xml", in: named(admissiontest.DOCXWith(admissiontest.ZipEntry{Name: "word/settings.xml", Data: []byte(`<x><y></x>`)}), "a.docx"), reason: ReasonMalformed},
		{name: "too many entries", in: named(admissiontest.ZipOf(manyEntries...), "a.docx"), reason: ReasonResourceLimit},
		{name: "zip bomb", in: named(zipWithRaw(t, admissiontest.DOCXEntries(docxMainContentType), rawZipEntry{"word/media/big.bin", 300 << 20, 300}), "a.docx"), reason: ReasonResourceLimit},
		{name: "zip entry lying about size", in: named(zipWithRaw(t, admissiontest.DOCXEntries(docxMainContentType), rawZipEntry{"word/media/big.bin", 1024, 4}), "a.docx"), reason: ReasonMalformed},
		{name: "main part missing", in: named(admissiontest.ZipOf(admissiontest.DOCXEntries(docxMainContentType)[:2]...), "a.docx"), reason: ReasonMalformed},
	})
}

func TestEvaluateImages(t *testing.T) {
	truncated := admissiontest.PNG()
	truncated = truncated[:len(truncated)-20]
	runEvalCases(t, []evalCase{
		{name: "png dimension bomb", in: named(pngHeaderOnly(100_000, 100_000), "a.png"), reason: ReasonImageTooLarge},
		{name: "png truncated", in: named(truncated, "a.png"), reason: ReasonMalformed},
		{name: "tiff too many pages", in: named(tiffChain(MaxTIFFPages+1), "a.tif"), reason: ReasonResourceLimit},
		{name: "tiff ifd cycle", in: named(tiffCycle(), "a.tif"), reason: ReasonMalformed},
	})
}

// hiddenZlib compresses content and asserts no forbidden name survives in
// the raw bytes, so only inflation can find it.
func hiddenZlib(t *testing.T, content string) string {
	compressed := string(zlibBytes(t, []byte(content)))
	require.NotContains(t, compressed, "/JS")
	return compressed
}

// forgedEndstreamObject hides /JS in a Flate stream after a literal
// "endstream" carried in a stored deflate block.
func forgedEndstreamObject(t *testing.T) string {
	var raw bytes.Buffer
	stored, err := flate.NewWriter(&raw, flate.NoCompression)
	require.NoError(t, err)
	_, err = stored.Write([]byte("endstream\nendobj\n"))
	require.NoError(t, err)
	require.NoError(t, stored.Flush())
	compressed, err := flate.NewWriter(&raw, flate.BestCompression)
	require.NoError(t, err)
	_, err = compressed.Write([]byte("<< /S /JavaScript /JS (app.alert(1)) >>"))
	require.NoError(t, err)
	require.NoError(t, compressed.Close())
	require.NotContains(t, raw.String(), "/JS", "the payload must be hidden from the raw scan")
	body := append([]byte{0x78, 0xDA}, raw.Bytes()...)
	return "<< /Filter /FlateDecode /Length 9 0 R >>\nstream\n" + string(body) + "\nendstream"
}

func tiffCycle() []byte {
	data := tiffChain(2)
	// Point the second IFD's next offset back at the first.
	copy(data[len(data)-4:], []byte{8, 0, 0, 0})
	return data
}

func TestNormalizeFilename(t *testing.T) {
	long := strings.Repeat("é", 200) + ".pdf"
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"report.pdf", "report.pdf", true},
		{"../../etc/passwd.txt", "passwd.txt", true},
		{`C:\Users\x\report.pdf`, "report.pdf", true},
		{" .hidden.txt. ", "hidden.txt", true},
		{"a\x00b\x1f\u0085c.txt", "abc.txt", true},
		{"acme.com.pdf", "acme.com.pdf", true},
		{"invoice\u202Etxt.exe", "", false},
		{"a\u2066b.pdf", "", false},
		{"invoice.pdf.exe", "", false},
		{"x.exe.pdf", "", false},
		{"x.EXE .pdf", "", false},
		{"bad\xffname.txt", "", false},
		{"..", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := NormalizeFilename(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	got, ok := NormalizeFilename(long)
	require.True(t, ok)
	assert.LessOrEqual(t, len(got), MaxFilenameBytes)
	assert.True(t, strings.HasSuffix(got, "é.pdf"))
}
