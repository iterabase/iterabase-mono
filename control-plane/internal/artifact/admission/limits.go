// Package admission implements the V2 artifact type and structural admission
// policy (DES-HOR-460-04 as amended by HOR-461).
//
// Trust boundary (DES-HOR-461-01): this package is project-owned policy code
// that runs only inside the isolated, credentialless scanner pod, and only
// after ClamAV has returned a clean verdict for the exact bytes. It detects the
// canonical type from content, applies bounded structural validation, turns an
// email HTML body into sanitised text, and expands .eml/.msg containers into
// raw children. It performs no I/O beyond its inputs (no network, filesystem,
// or exec), is deterministic, and bounds memory and CPU on hostile input.
//
// Children are returned raw and are never evaluated here: the caller admits
// each child as its own artifact, evaluated separately at ContainerDepth+1.
package admission

// Hard limits from the architecture contract (section 6). They are not
// configurable: changing them is a product and architecture decision.
const (
	// MaxFileBytes is 25 MiB, the per-artifact byte limit (and per-child).
	MaxFileBytes = 26_214_400
	// MaxChildren bounds the children a container may expand into.
	MaxChildren = 50
	// MaxExpandedBytes bounds the aggregate decoded/decompressed bytes examined.
	MaxExpandedBytes = 256 << 20
	// MaxPackageEntries bounds ZIP entries, CFB directory entries, and MIME parts.
	MaxPackageEntries = 10_000
	// MaxRecursion bounds nested MIME multipart recursion.
	MaxRecursion = 16
	// MaxPixelsPerImage bounds the pixels of one image or TIFF page.
	MaxPixelsPerImage = 50_000_000
	// MaxTIFFPages bounds the pages (IFDs) of one TIFF.
	MaxTIFFPages = 250
	// MaxAggregatePixels bounds the pixels across all pages of one TIFF.
	MaxAggregatePixels = 500_000_000
	// MaxPDFStreamBytes bounds one inflated PDF stream.
	MaxPDFStreamBytes = 64 << 20
	// MaxCSVCells bounds the cells of one CSV.
	MaxCSVCells = 1_000_000
	// MaxCSVFieldBytes bounds one CSV field.
	MaxCSVFieldBytes = 1 << 20
	// MaxFilenameBytes bounds a normalised display filename.
	MaxFilenameBytes = 255
)

// Canonical MIME types of the V2 allow-list.
const (
	MIMEPDF  = "application/pdf"
	MIMEDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MIMECSV  = "text/csv"
	MIMEXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMETXT  = "text/plain"
	MIMEJPEG = "image/jpeg"
	MIMEPNG  = "image/png"
	MIMETIFF = "image/tiff"
	MIMEEML  = "message/rfc822"
	MIMEMSG  = "application/vnd.ms-outlook"
)

// Rejection reason codes. They are stable, customer-safe, and bounded.
const (
	ReasonUnsupportedType       = "unsupported_type"
	ReasonTypeMismatch          = "type_mismatch"
	ReasonEncrypted             = "encrypted"
	ReasonActiveContent         = "active_content"
	ReasonMacroContent          = "macro_content"
	ReasonEmbeddedObject        = "embedded_object"
	ReasonExternalDependency    = "external_dependency"
	ReasonMalformed             = "malformed"
	ReasonInvalidTextEncoding   = "invalid_text_encoding"
	ReasonImageTooLarge         = "image_too_large"
	ReasonResourceLimit         = "resource_limit"
	ReasonContainerTooManyParts = "container_too_many_parts"
	ReasonEmptyFile             = "empty_file"
	ReasonUnsafeFilename        = "unsafe_filename"
)

// budget tracks the aggregate expanded bytes examined by one evaluation.
type budget struct{ remaining int64 }

func newBudget() *budget { return &budget{remaining: MaxExpandedBytes} }

// take reserves n bytes and reports whether the budget allowed it.
func (b *budget) take(n int64) bool {
	if n < 0 || n > b.remaining {
		b.remaining = 0
		return false
	}
	b.remaining -= n
	return true
}
