package admission

import (
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

var knownReasons = map[string]bool{
	ReasonUnsupportedType: true, ReasonTypeMismatch: true, ReasonEncrypted: true,
	ReasonActiveContent: true, ReasonMacroContent: true, ReasonEmbeddedObject: true,
	ReasonExternalDependency: true, ReasonMalformed: true, ReasonInvalidTextEncoding: true,
	ReasonImageTooLarge: true, ReasonResourceLimit: true, ReasonContainerTooManyParts: true,
	ReasonEmptyFile: true, ReasonUnsafeFilename: true,
}

func FuzzEvaluate(f *testing.F) {
	seeds := []struct {
		data     []byte
		filename string
	}{
		{admissiontest.PDF(), "a.pdf"},
		{admissiontest.PDFWith(flateStream(f, "<< /JS (x) >>")), "a.pdf"},
		{admissiontest.DOCX(), "a.docx"},
		{admissiontest.XLSX(), "a.xlsx"},
		{[]byte("a,b\n1,2\n"), "a.csv"},
		{[]byte("hello"), "a.txt"},
		{admissiontest.PNG(), "a.png"},
		{admissiontest.JPEG(), "a.jpg"},
		{admissiontest.TIFF(), "a.tif"},
		{multipartMessage("mixed", mimePart("Content-Type: text/html", "<p>x</p>"), pdfAttachmentPart(f, "a.pdf")), "a.eml"},
		{admissiontest.BuildMSG("Quarterly report", "body", []byte("<p>x</p>"), admissiontest.MSGAttachment{Filename: "a.pdf", Data: admissiontest.PDF()}), "a.msg"},
	}
	for _, seed := range seeds {
		f.Add(seed.data, seed.filename, "", false, 1)
	}
	f.Fuzz(func(t *testing.T, data []byte, filename, claim string, generated bool, depth int) {
		got := Evaluate(Input{Data: data, Filename: filename, ClaimedMIME: claim, Generated: generated, ContainerDepth: depth % 4})
		if got.Accepted {
			if got.CanonicalMIME == "" || got.Reason != "" {
				t.Fatalf("inconsistent accept: %+v", got)
			}
		} else if !knownReasons[got.Reason] || got.CanonicalMIME != "" || got.Children != nil {
			t.Fatalf("inconsistent reject: %+v", got)
		}
		if len(got.Children) > MaxChildren {
			t.Fatalf("children %d exceed limit", len(got.Children))
		}
		for i, child := range got.Children {
			if child.Index != i || len(child.Data) > MaxFileBytes || child.Filename == "" {
				t.Fatalf("invalid child %d: index=%d size=%d name=%q", i, child.Index, len(child.Data), child.Filename)
			}
		}
	})
}
