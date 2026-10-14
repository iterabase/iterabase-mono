package admission

import (
	"bytes"
	"mime"
	"strings"
)

// Input is one candidate artifact. Data is complete, at most MaxFileBytes, and
// already malware-clean. ClaimedMIME and Filename are advisory claims.
type Input struct {
	Data           []byte
	ClaimedMIME    string
	Filename       string
	Generated      bool
	ContainerDepth int
}

// ChildKind distinguishes an email body from an attachment.
type ChildKind string

const (
	// ChildBody is the sanitised UTF-8 text body (canonical text/plain).
	ChildBody ChildKind = "body"
	// ChildAttachment is one real attachment, returned raw.
	ChildAttachment ChildKind = "attachment"
)

// BodyFilename is the display filename of every email body child.
const BodyFilename = "message-body.txt"

// Child is one raw part of an expanded container.
type Child struct {
	Kind        ChildKind
	Index       int
	Filename    string
	ClaimedMIME string
	Data        []byte
	// SourceHTML is set only on a body derived from HTML: the original HTML
	// part bytes, kept as quarantined evidence and never rendered.
	SourceHTML []byte
}

// Result is the admission verdict. CanonicalMIME is set when Accepted, Reason
// when not. Children are set only for an expanded container.
type Result struct {
	Accepted      bool
	CanonicalMIME string
	Reason        string
	Children      []Child
}

func accept(canonical string) Result { return Result{Accepted: true, CanonicalMIME: canonical} }

func reject(reason string) Result { return Result{Reason: reason} }

var extensionMIME = map[string]string{
	"pdf": MIMEPDF, "docx": MIMEDOCX, "xlsx": MIMEXLSX, "csv": MIMECSV, "txt": MIMETXT,
	"jpg": MIMEJPEG, "jpeg": MIMEJPEG, "png": MIMEPNG, "tif": MIMETIFF, "tiff": MIMETIFF,
	"eml": MIMEEML, "msg": MIMEMSG,
}

var claimAliases = map[string]string{
	"image/jpg":                   MIMEJPEG,
	"image/pjpeg":                 MIMEJPEG,
	"application/x-pdf":           MIMEPDF,
	"text/comma-separated-values": MIMECSV,
	"application/csv":             MIMECSV,
	"image/tif":                   MIMETIFF,
	"application/x-msg":           MIMEMSG,
}

// Evaluate applies the complete admission policy to one candidate artifact.
// It is deterministic and performs no I/O.
func Evaluate(in Input) Result {
	if len(in.Data) > MaxFileBytes {
		return reject(ReasonResourceLimit)
	}
	name, ok := normalizeFilename(in.Filename)
	if !ok || (name == "" && !in.Generated) {
		return reject(ReasonUnsafeFilename)
	}
	if len(in.Data) == 0 {
		return reject(ReasonEmptyFile)
	}
	extMIME := ""
	if ext := extensionOf(name); ext != "" {
		if extMIME, ok = extensionMIME[ext]; !ok {
			return reject(ReasonUnsupportedType)
		}
	} else if !in.Generated {
		return reject(ReasonUnsupportedType)
	}
	canonical, reason := detect(in.Data, extMIME)
	if reason != "" {
		return reject(reason)
	}
	if extMIME != canonical && (extMIME != "" || canonical != MIMETXT) {
		return reject(ReasonTypeMismatch)
	}
	if !claimCompatible(in.ClaimedMIME, canonical) {
		return reject(ReasonTypeMismatch)
	}
	return validate(canonical, in)
}

func validate(canonical string, in Input) Result {
	var reason string
	switch canonical {
	case MIMEPDF:
		reason = validatePDF(in.Data)
	case MIMEDOCX, MIMEXLSX:
		reason = validateOOXML(in.Data, canonical)
	case MIMECSV, MIMETXT:
		reason = validateText(in.Data, canonical == MIMECSV)
	case MIMEJPEG, MIMEPNG, MIMETIFF:
		reason = validateImage(in.Data, canonical)
	case MIMEEML:
		return evaluateEML(in)
	case MIMEMSG:
		return evaluateMSG(in)
	default:
		reason = ReasonUnsupportedType
	}
	if reason != "" {
		return reject(reason)
	}
	return accept(canonical)
}

// detect derives the canonical type from content. extMIME only decides
// between the textual families, which share no magic bytes.
func detect(data []byte, extMIME string) (string, string) {
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return detectPackage(data)
	case bytes.HasPrefix(data, cfbMagic):
		return detectCFB(data)
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return MIMEJPEG, ""
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG, ""
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return MIMETIFF, ""
	case isForbiddenBinary(data):
		return "", ReasonUnsupportedType
	case bytes.Contains(head(data, 1024), []byte("%PDF-")):
		// Readers accept the PDF header anywhere in the first KiB; any
		// format with an offset-0 signature is classified by it first.
		return MIMEPDF, ""
	}
	switch extMIME {
	case MIMEEML:
		return MIMEEML, ""
	case MIMECSV:
		return MIMECSV, ""
	case MIMETXT, "":
		return MIMETXT, ""
	}
	if looksTextual(data) {
		return "", ReasonTypeMismatch
	}
	return "", ReasonUnsupportedType
}

// forbiddenPrefixes are recognised formats outside the V2 allow-list:
// executables, scripts, archives, and unsupported images.
var forbiddenPrefixes = [][]byte{
	[]byte("\x7fELF"),
	{0xFE, 0xED, 0xFA, 0xCE}, {0xFE, 0xED, 0xFA, 0xCF},
	{0xCE, 0xFA, 0xED, 0xFE}, {0xCF, 0xFA, 0xED, 0xFE},
	{0xCA, 0xFE, 0xBA, 0xBE},
	[]byte("#!"),
	[]byte("\x00asm"),
	[]byte("GIF87a"), []byte("GIF89a"),
	{0x1F, 0x8B},
	[]byte("7z\xBC\xAF\x27\x1C"),
	[]byte("Rar!\x1A\x07"),
	[]byte("BZh"),
	{0xFD, '7', 'z', 'X', 'Z', 0x00},
	{0x28, 0xB5, 0x2F, 0xFD},
	[]byte("PK\x05\x06"), []byte("PK\x07\x08"),
	[]byte("II+\x00"), []byte("MM\x00+"),
}

func isForbiddenBinary(data []byte) bool {
	for _, prefix := range forbiddenPrefixes {
		if bytes.HasPrefix(data, prefix) {
			return true
		}
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return true
	}
	if len(data) >= 262 && bytes.Equal(data[257:262], []byte("ustar")) {
		return true
	}
	// MZ (PE) and BM (BMP) are also plausible text openings; their binary
	// headers always carry NUL bytes early.
	if bytes.HasPrefix(data, []byte("MZ")) || bytes.HasPrefix(data, []byte("BM")) {
		return bytes.IndexByte(head(data, 64), 0) >= 0
	}
	return false
}

func claimCompatible(claim, canonical string) bool {
	normalized := normalizeMIME(claim)
	if normalized == "" || normalized == "application/octet-stream" {
		return true
	}
	if alias, ok := claimAliases[normalized]; ok {
		normalized = alias
	}
	return normalized == canonical
}

// normalizeMIME returns the lowercased media type without parameters.
func normalizeMIME(value string) string {
	if mediaType, _, err := mime.ParseMediaType(value); err == nil {
		return mediaType
	}
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func head(data []byte, n int) []byte {
	if len(data) < n {
		return data
	}
	return data[:n]
}
