package admissiontest

// Expected canonical types and reason codes, as literals: this package does
// not import admission (see the package documentation).
const (
	mimePDF  = "application/pdf"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimeCSV  = "text/csv"
	mimeTXT  = "text/plain"
	mimeJPEG = "image/jpeg"
	mimePNG  = "image/png"
	mimeTIFF = "image/tiff"
	mimeEML  = "message/rfc822"
	mimeMSG  = "application/vnd.ms-outlook"

	reasonUnsupportedType     = "unsupported_type"
	reasonTypeMismatch        = "type_mismatch"
	reasonEncrypted           = "encrypted"
	reasonActiveContent       = "active_content"
	reasonMacroContent        = "macro_content"
	reasonEmbeddedObject      = "embedded_object"
	reasonInvalidTextEncoding = "invalid_text_encoding"
)

// Case is one corpus entry and its expected admission result. Depth is the
// container depth to evaluate at; zero means 1 (a directly uploaded file).
type Case struct {
	Name          string
	Filename      string
	ClaimedMIME   string
	Data          []byte
	Accepted      bool
	CanonicalMIME string
	Reason        string
	Children      int
	Depth         int
}

// Corpus returns every allowed type (accepted) and representative
// rejections, built fresh on each call.
func Corpus() []Case {
	attachedMessage := EML("Original message", "Forwarded body.\n", "")
	return []Case{
		{Name: "pdf", Filename: "report.pdf", ClaimedMIME: mimePDF, Data: PDF(), Accepted: true, CanonicalMIME: mimePDF},
		{Name: "docx", Filename: "letter.docx", ClaimedMIME: mimeDOCX, Data: DOCX(), Accepted: true, CanonicalMIME: mimeDOCX},
		{Name: "xlsx", Filename: "book.xlsx", ClaimedMIME: mimeXLSX, Data: XLSX(), Accepted: true, CanonicalMIME: mimeXLSX},
		{Name: "csv", Filename: "data.csv", ClaimedMIME: mimeCSV, Data: CSV(), Accepted: true, CanonicalMIME: mimeCSV},
		{Name: "txt", Filename: "notes.txt", ClaimedMIME: mimeTXT, Data: TXT(), Accepted: true, CanonicalMIME: mimeTXT},
		{Name: "jpeg", Filename: "photo.jpg", ClaimedMIME: mimeJPEG, Data: JPEG(), Accepted: true, CanonicalMIME: mimeJPEG},
		{Name: "png", Filename: "scan.png", ClaimedMIME: mimePNG, Data: PNG(), Accepted: true, CanonicalMIME: mimePNG},
		{Name: "tiff", Filename: "scan.tif", ClaimedMIME: mimeTIFF, Data: TIFF(), Accepted: true, CanonicalMIME: mimeTIFF},
		{
			Name: "eml with pdf and attached eml", Filename: "mail.eml", ClaimedMIME: mimeEML,
			Data: EML("Quarterly report", "Please find the report attached.\n", "",
				Attachment{Filename: "report.pdf", ContentType: mimePDF, Data: PDF()},
				Attachment{Filename: "original.eml", ContentType: mimeEML, Data: attachedMessage}),
			Accepted: true, CanonicalMIME: mimeEML, Children: 3,
		},
		{
			Name: "msg with pdf", Filename: "mail.msg", ClaimedMIME: mimeMSG,
			Data:     MSG("Quarterly report", "Please find the report attached.", Attachment{Filename: "report.pdf", ContentType: mimePDF, Data: PDF()}),
			Accepted: true, CanonicalMIME: mimeMSG, Children: 2,
		},
		{Name: "claimed type mismatch", Filename: "report.pdf", ClaimedMIME: mimePNG, Data: PDF(), Reason: reasonTypeMismatch},
		{Name: "renamed exe as pdf", Filename: "invoice.pdf", ClaimedMIME: mimePDF, Data: EXE(), Reason: reasonUnsupportedType},
		{Name: "generic zip as docx", Filename: "letter.docx", ClaimedMIME: mimeDOCX, Data: ZIP(), Reason: reasonUnsupportedType},
		{Name: "macro docx", Filename: "letter.docx", ClaimedMIME: mimeDOCX, Data: MacroDOCX(), Reason: reasonMacroContent},
		{Name: "active pdf", Filename: "report.pdf", ClaimedMIME: mimePDF, Data: ActivePDF(), Reason: reasonActiveContent},
		{Name: "encrypted pdf", Filename: "report.pdf", ClaimedMIME: mimePDF, Data: EncryptedPDF(), Reason: reasonEncrypted},
		{Name: "encrypted ooxml as docx", Filename: "letter.docx", ClaimedMIME: mimeDOCX, Data: EncryptedOOXML(), Reason: reasonEncrypted},
		{Name: "html as txt", Filename: "notes.txt", ClaimedMIME: mimeTXT, Data: HTML(), Reason: reasonUnsupportedType},
		{Name: "invalid utf-8 txt", Filename: "notes.txt", ClaimedMIME: mimeTXT, Data: []byte("caf\xe9\n"), Reason: reasonInvalidTextEncoding},
		{Name: "msg with ole attachment", Filename: "mail.msg", ClaimedMIME: mimeMSG, Data: MSGWithOLEAttachment(), Reason: reasonEmbeddedObject},
	}
}
