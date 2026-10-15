// Package admissiontest builds the deterministic artifact admission fixture
// corpus in code, so unit, integration, and E2E tests share the exact same
// bytes. It deliberately does not import the admission package (whose own
// tests use it); Corpus therefore carries the expected MIME types and reason
// codes as literals, and the admission package's TestCorpus pins them.
//
// Every builder is deterministic. Builders panic only on internal errors
// that cannot happen when writing to memory.
package admissiontest

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"

	"golang.org/x/image/tiff"
)

// MalwareMarker is a benign marker string that test malware engines (the
// fake scanner used by integration and E2E tests) treat as malware.
const MalwareMarker = "ITERABASE-TEST-MALWARE-MARKER-7f3c"

// WithMarker returns a copy of data with MalwareMarker appended.
func WithMarker(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(MalwareMarker))
	out = append(out, data...)
	return append(out, MalwareMarker...)
}

func must(err error) {
	if err != nil {
		panic("admissiontest: " + err.Error())
	}
}

// PDF document fixtures.

const pdfText = "BT /F1 12 Tf 72 720 Td (Iterabase admission fixture) Tj ET"

var pdfBaseObjects = []string{
	"<< /Type /Catalog /Pages 2 0 R >>",
	"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
	"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
	fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(pdfText), pdfText),
	"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
}

// pdfDoc builds a minimal PDF with a valid xref from object bodies.
func pdfDoc(objects ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// PDF returns a valid one-page PDF with a single line of text.
func PDF() []byte { return pdfDoc(pdfBaseObjects...) }

// PDFWith returns PDF with extra indirect objects (numbered from 6) appended.
func PDFWith(extra ...string) []byte {
	return pdfDoc(append(append([]string{}, pdfBaseObjects...), extra...)...)
}

// ActivePDF returns a PDF carrying a JavaScript action.
func ActivePDF() []byte { return PDFWith("<< /S /JavaScript /JS (app.alert(1)) >>") }

// EncryptedPDF returns a PDF carrying an /Encrypt dictionary.
func EncryptedPDF() []byte {
	return PDFWith("<< /Filter /Standard /V 2 /R 3 /Length 128 >>", "<< /Encrypt 6 0 R >>")
}

// EncodedActivePDF returns a PDF whose JavaScript action dictionary lives
// only inside an ASCIIHexDecode object stream, so no raw byte spells it.
func EncodedActivePDF() []byte {
	objects := "6 0 << /S /JavaScript /JS (app.alert(1)) >>"
	encoded := fmt.Sprintf("%X>", objects)
	return PDFWith(fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Filter /ASCIIHexDecode /Length %d >>\nstream\n%s\nendstream",
		len(encoded), encoded))
}

// ZIP and OOXML fixtures.

// ZipEntry is one stored file of a ZIP fixture.
type ZipEntry struct {
	Name string
	Data []byte
}

// ZipOf returns a ZIP archive holding entries in order (no timestamps).
func ZipOf(entries ...ZipEntry) []byte {
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, e := range entries {
		f, err := w.Create(e.Name)
		must(err)
		_, err = f.Write(e.Data)
		must(err)
	}
	must(w.Close())
	return out.Bytes()
}

// ZIP returns a generic (non-OOXML) ZIP archive.
func ZIP() []byte {
	return ZipOf(ZipEntry{"readme.txt", []byte("not an office document\n")})
}

// OOXML package constants.
const (
	// DOCXMainContentType is the content type of a macro-free DOCX main part.
	DOCXMainContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
	// XLSXMainContentType is the content type of a macro-free XLSX main part.
	XLSXMainContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	// OfficeRelationships is the officeDocument relationship namespace.
	OfficeRelationships = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

	contentTypesPart = "[Content_Types].xml"
	relsNS           = "http://schemas.openxmlformats.org/package/2006/relationships"
)

func contentTypesXML(mainPart, mainType string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="` + mainPart + `" ContentType="` + mainType + `"/></Types>`
}

// RelsXML returns a relationships part holding the given Relationship elements.
func RelsXML(rels ...string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + relsNS + `">` +
		strings.Join(rels, "") + `</Relationships>`
}

// DOCXEntries returns the entries of a minimal DOCX whose main part has
// mainContentType, followed by extra.
func DOCXEntries(mainContentType string, extra ...ZipEntry) []ZipEntry {
	entries := []ZipEntry{
		{contentTypesPart, []byte(contentTypesXML("/word/document.xml", mainContentType))},
		{"_rels/.rels", []byte(RelsXML(`<Relationship Id="rId1" Type="` + OfficeRelationships + `/officeDocument" Target="word/document.xml"/>`))},
		{"word/document.xml", []byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello</w:t></w:r></w:p></w:body></w:document>`)},
	}
	return append(entries, extra...)
}

// DOCXWith returns a minimal DOCX with extra package entries appended.
func DOCXWith(extra ...ZipEntry) []byte {
	return ZipOf(DOCXEntries(DOCXMainContentType, extra...)...)
}

// DOCX returns a minimal valid Word document.
func DOCX() []byte { return DOCXWith() }

// MacroDOCX returns a DOCX carrying a VBA project (word/vbaProject.bin).
func MacroDOCX() []byte {
	return DOCXWith(ZipEntry{"word/vbaProject.bin", []byte("vba")})
}

// XLSX returns a minimal valid Excel workbook.
func XLSX() []byte {
	return ZipOf(
		ZipEntry{contentTypesPart, []byte(contentTypesXML("/xl/workbook.xml", XLSXMainContentType))},
		ZipEntry{"_rels/.rels", []byte(RelsXML(`<Relationship Id="rId1" Type="` + OfficeRelationships + `/officeDocument" Target="xl/workbook.xml"/>`))},
		ZipEntry{"xl/workbook.xml", []byte(`<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets/></workbook>`)},
	)
}

// EncryptedOOXML returns a password-protected OOXML container: a compound
// file holding EncryptionInfo and EncryptedPackage streams.
func EncryptedOOXML() []byte {
	return CFB(
		CFBStream("EncryptionInfo", []byte("\x04\x00\x04\x00\x40\x00\x00\x00")),
		CFBStream("EncryptedPackage", bytes.Repeat([]byte{0xA5}, 64)),
	)
}

// Text fixtures.

// CSV returns a small UTF-8 CSV.
func CSV() []byte { return []byte("name,amount\nwidget,3\n\"gadget, large\",5\n") }

// TXT returns a small UTF-8 plain text file.
func TXT() []byte { return []byte("Iterabase admission fixture.\nSecond line.\n") }

// HTML returns an HTML document (outside the allow-list).
func HTML() []byte {
	return []byte("<!DOCTYPE html><html><head><title>x</title><script>alert(1)</script></head><body>hi</body></html>")
}

// EXE returns the minimal header bytes of a PE executable: a DOS MZ header
// whose e_lfanew points at a PE signature and an amd64 COFF header.
func EXE() []byte {
	out := make([]byte, 0x40+4+20)
	copy(out, "MZ")
	binary.LittleEndian.PutUint16(out[2:], 0x0090)
	binary.LittleEndian.PutUint32(out[0x3C:], 0x40)
	copy(out[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(out[0x44:], 0x8664)
	binary.LittleEndian.PutUint16(out[0x46:], 1)
	return out
}

// Image fixtures.

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for x := range 8 {
		img.Set(x, 3, color.RGBA{R: 200, A: 255})
	}
	return img
}

// PNG returns a small valid PNG.
func PNG() []byte {
	var out bytes.Buffer
	must(png.Encode(&out, testImage()))
	return out.Bytes()
}

// JPEG returns a small valid JPEG.
func JPEG() []byte {
	var out bytes.Buffer
	must(jpeg.Encode(&out, testImage(), nil))
	return out.Bytes()
}

// TIFF returns a small valid single-page TIFF.
func TIFF() []byte {
	var out bytes.Buffer
	must(tiff.Encode(&out, testImage(), nil))
	return out.Bytes()
}
