package admission

import (
	"io"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// ExpandableDepth is the deepest ContainerDepth whose container is expanded.
// A mailbox message is depth 0 and an uploaded .eml/.msg depth 1; a container
// found inside either is depth 2 and is accepted unexpanded (no children).
const ExpandableDepth = 1

// AttachedMessageFilename names an attached message that carries no filename.
const AttachedMessageFilename = "attached-message.eml"

var canonicalExtension = map[string]string{
	MIMEPDF: ".pdf", MIMEDOCX: ".docx", MIMEXLSX: ".xlsx", MIMECSV: ".csv", MIMETXT: ".txt",
	MIMEJPEG: ".jpg", MIMEPNG: ".png", MIMETIFF: ".tif", MIMEEML: ".eml", MIMEMSG: ".msg",
}

// wordDecoder decodes RFC 2047 encoded-words in any WHATWG-labelled charset.
var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(input), nil
}}

// containerBuilder accumulates children in order: the body first, then the
// attachments in document order.
type containerBuilder struct {
	children []Child
}

func (c *containerBuilder) addBody(text string, sourceHTML []byte) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.children = append(c.children, Child{
		Kind: ChildBody, Index: len(c.children), Filename: BodyFilename,
		ClaimedMIME: MIMETXT, Data: []byte(text), SourceHTML: sourceHTML,
	})
}

func (c *containerBuilder) addAttachment(filename, claimedMIME string, data []byte) {
	index := len(c.children)
	c.children = append(c.children, Child{
		Kind: ChildAttachment, Index: index, Filename: childFilename(filename, index, claimedMIME),
		ClaimedMIME: claimedMIME, Data: data,
	})
}

// result is all-or-nothing: too many parts or one oversized child rejects the
// whole container and returns no children.
func (c *containerBuilder) result(canonical string) Result {
	if len(c.children) > MaxChildren {
		return reject(ReasonContainerTooManyParts)
	}
	for _, child := range c.children {
		if len(child.Data) > MaxFileBytes {
			return reject(ReasonResourceLimit)
		}
	}
	result := accept(canonical)
	result.Children = c.children
	return result
}

// childFilename normalises a child's claimed filename, replacing a missing or
// unsafe one with a generated "attachment-<n>" name.
func childFilename(raw string, index int, claimedMIME string) string {
	if name, ok := NormalizeFilename(raw); ok {
		return name
	}
	mediaType := normalizeMIME(claimedMIME)
	if mediaType == MIMEEML && raw == "" {
		return AttachedMessageFilename
	}
	if alias, ok := claimAliases[mediaType]; ok {
		mediaType = alias
	}
	return "attachment-" + strconv.Itoa(index) + canonicalExtension[mediaType]
}

// decodeCharset converts text in the labelled charset to valid UTF-8. An
// unlabelled or ASCII-labelled 8-bit text falls back to Windows-1252.
func decodeCharset(label string, data []byte) (string, string) {
	switch label = strings.ToLower(strings.TrimSpace(label)); label {
	case "utf-8", "utf8":
		if !utf8.Valid(data) {
			return "", ReasonInvalidTextEncoding
		}
		return string(data), ""
	case "", "us-ascii", "ascii":
		if utf8.Valid(data) {
			return string(data), ""
		}
		label = "windows-1252"
	}
	enc, err := htmlindex.Get(label)
	if err != nil {
		return "", ReasonMalformed
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil || !utf8.Valid(out) {
		return "", ReasonInvalidTextEncoding
	}
	return string(out), ""
}

// decodeLegacyString decodes an 8-bit string as UTF-8 when valid, else as
// Windows-1252 (the dominant legacy Outlook code page).
func decodeLegacyString(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	out, err := charmap.Windows1252.NewDecoder().Bytes(data)
	if err != nil {
		return ""
	}
	return string(out)
}
