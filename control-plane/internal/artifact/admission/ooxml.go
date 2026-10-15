package admission

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

const (
	docxMainContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"
	xlsxMainContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	contentTypesPart    = "[Content_Types].xml"
	hyperlinkRelSuffix  = "/relationships/hyperlink"
)

var errBudgetExceeded = errors.New("admission: expanded byte budget exceeded")

type contentTypes struct {
	Defaults []struct {
		Extension   string `xml:"Extension,attr"`
		ContentType string `xml:"ContentType,attr"`
	} `xml:"Default"`
	Overrides []struct {
		PartName    string `xml:"PartName,attr"`
		ContentType string `xml:"ContentType,attr"`
	} `xml:"Override"`
}

// all returns every declared content type in document order.
func (c *contentTypes) all() []string {
	types := make([]string, 0, len(c.Defaults)+len(c.Overrides))
	for _, d := range c.Defaults {
		types = append(types, d.ContentType)
	}
	for _, o := range c.Overrides {
		types = append(types, o.ContentType)
	}
	return types
}

// detectPackage classifies a ZIP: DOCX, XLSX, a macro-enabled variant, or a
// generic (unsupported) archive.
func detectPackage(data []byte) (string, string) {
	zr, reason := openPackage(data)
	if reason != "" {
		return "", reason
	}
	types, reason := readContentTypes(zr, newBudget())
	if reason != "" {
		return "", reason
	}
	docx, xlsx := false, false
	for _, contentType := range types.all() {
		switch {
		case strings.Contains(strings.ToLower(contentType), "macroenabled"):
			return "", ReasonMacroContent
		case contentType == docxMainContentType:
			docx = true
		case contentType == xlsxMainContentType:
			xlsx = true
		}
	}
	switch {
	case docx && !xlsx:
		return MIMEDOCX, ""
	case xlsx && !docx:
		return MIMEXLSX, ""
	}
	return "", ReasonUnsupportedType
}

func openPackage(data []byte) (*zip.Reader, string) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ReasonMalformed
	}
	if len(zr.File) > MaxPackageEntries {
		return nil, ReasonResourceLimit
	}
	var declared uint64
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		if declared > MaxExpandedBytes {
			return nil, ReasonResourceLimit
		}
	}
	return zr, ""
}

// readContentTypes reads [Content_Types].xml; a ZIP without it is not OOXML.
func readContentTypes(zr *zip.Reader, b *budget) (*contentTypes, string) {
	for _, f := range zr.File {
		if f.Name != contentTypesPart {
			continue
		}
		data, reason := readEntry(f, b)
		if reason != "" {
			return nil, reason
		}
		var types contentTypes
		if err := xml.Unmarshal(data, &types); err != nil {
			return nil, ReasonMalformed
		}
		return &types, ""
	}
	return nil, ReasonUnsupportedType
}

// validateOOXML applies the bounded DOCX/XLSX package policy.
func validateOOXML(data []byte, canonical string) string {
	zr, reason := openPackage(data)
	if reason != "" {
		return reason
	}
	b := newBudget()
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return ReasonEncrypted
		}
		if reason := partNameReason(f.Name); reason != "" {
			return reason
		}
		names["/"+f.Name] = true
	}
	types, reason := readContentTypes(zr, b)
	if reason != "" {
		return reason
	}
	if reason := contentTypesReason(types, canonical, names); reason != "" {
		return reason
	}
	for _, f := range zr.File {
		if reason := inspectPart(f, b); reason != "" {
			return reason
		}
	}
	return ""
}

func partNameReason(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "vbaproject") || strings.Contains(lower, "vbadata"):
		return ReasonMacroContent
	case strings.Contains(lower, "embeddings/"), strings.Contains(lower, "activex"),
		strings.Contains(lower, "oleobject"):
		return ReasonEmbeddedObject
	}
	return ""
}

func contentTypesReason(types *contentTypes, canonical string, names map[string]bool) string {
	for _, contentType := range types.all() {
		lower := strings.ToLower(contentType)
		switch {
		case strings.Contains(lower, "vba"), strings.Contains(lower, "macroenabled"):
			return ReasonMacroContent
		case strings.Contains(lower, "oleobject"), strings.Contains(lower, "activex"),
			strings.Contains(lower, "officedocument.package"):
			return ReasonEmbeddedObject
		}
	}
	want := docxMainContentType
	if canonical == MIMEXLSX {
		want = xlsxMainContentType
	}
	for _, o := range types.Overrides {
		if o.ContentType == want && names[o.PartName] {
			return ""
		}
	}
	return ReasonMalformed
}

// inspectPart streams one part within the budget: XML parts are tokenised
// (no DOCTYPE, no non-hyperlink external relationship); other parts must not
// be a nested archive or compound file.
func inspectPart(f *zip.File, b *budget) string {
	rc, err := f.Open()
	if err != nil {
		return ReasonMalformed
	}
	defer func() { _ = rc.Close() }()
	reader := &budgetReader{r: rc, b: b}
	lower := strings.ToLower(f.Name)
	var reason string
	if strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".rels") {
		reason = inspectXML(reader, strings.HasSuffix(lower, ".rels"))
	} else {
		reason = inspectBinaryPart(reader)
	}
	if reader.exceeded {
		return ReasonResourceLimit
	}
	return reason
}

func inspectXML(r io.Reader, isRels bool) string {
	decoder := xml.NewDecoder(r)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ReasonMalformed
		}
		switch t := token.(type) {
		case xml.Directive:
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(string(t))), "DOCTYPE") {
				return ReasonMalformed
			}
		case xml.StartElement:
			if isRels && t.Name.Local == "Relationship" && isExternalDependency(t.Attr) {
				return ReasonExternalDependency
			}
		}
	}
}

func isExternalDependency(attrs []xml.Attr) bool {
	external, relType := false, ""
	for _, attr := range attrs {
		switch attr.Name.Local {
		case "TargetMode":
			external = strings.EqualFold(attr.Value, "External")
		case "Type":
			relType = attr.Value
		}
	}
	return external && !strings.HasSuffix(relType, hyperlinkRelSuffix)
}

func inspectBinaryPart(r io.Reader) string {
	prefix := make([]byte, len(cfbMagic))
	n, err := io.ReadFull(r, prefix)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return ReasonMalformed
	}
	prefix = prefix[:n]
	if bytes.HasPrefix(prefix, []byte("PK\x03\x04")) || bytes.Equal(prefix, cfbMagic) {
		return ReasonEmbeddedObject
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return ReasonMalformed
	}
	return ""
}

func readEntry(f *zip.File, b *budget) ([]byte, string) {
	rc, err := f.Open()
	if err != nil {
		return nil, ReasonMalformed
	}
	defer func() { _ = rc.Close() }()
	reader := &budgetReader{r: rc, b: b}
	data, err := io.ReadAll(reader)
	if reader.exceeded {
		return nil, ReasonResourceLimit
	}
	if err != nil {
		return nil, ReasonMalformed
	}
	return data, ""
}

// budgetReader charges every byte read against the aggregate budget, so a
// lying ZIP header cannot inflate past MaxExpandedBytes.
type budgetReader struct {
	r        io.Reader
	b        *budget
	exceeded bool
}

func (br *budgetReader) Read(p []byte) (int, error) {
	n, err := br.r.Read(p)
	if !br.b.take(int64(n)) {
		br.exceeded = true
		return n, errBudgetExceeded
	}
	return n, err
}
