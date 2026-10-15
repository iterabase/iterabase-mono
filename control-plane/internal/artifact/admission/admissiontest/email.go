package admissiontest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"
	"unicode/utf16"
)

// Attachment is one attached file of an EML or MSG fixture.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// mimeRFC822 is the content type of an attached message.
const mimeRFC822 = "message/rfc822"

// EML returns a deterministic RFC 5322 message. The body is text/plain,
// text/html, or multipart/alternative when both are non-empty (none when
// both are empty); with attachments it is wrapped in multipart/mixed. Each
// attachment is base64 with Content-Disposition attachment, except a
// message/rfc822 attachment, which is embedded raw as an attached message.
// Text bodies are quoted-printable, so line breaks become CRLF.
func EML(subject, textBody, htmlBody string, attachments ...Attachment) []byte {
	var out bytes.Buffer
	out.WriteString("From: Sender <sender@example.com>\r\nTo: Recipient <recipient@example.com>\r\n")
	out.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	out.WriteString("Date: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <fixture@iterabase.test>\r\nMIME-Version: 1.0\r\n")
	var parts []string
	if body := emlBody(textBody, htmlBody); body != "" {
		parts = append(parts, body)
	}
	for _, a := range attachments {
		parts = append(parts, emlAttachment(a))
	}
	switch {
	case len(attachments) > 0:
		out.WriteString(multipart("mixed", parts))
	case len(parts) == 1:
		out.WriteString(parts[0])
	default:
		out.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	}
	return out.Bytes()
}

// emlBody returns the body entity (headers, blank line, content).
func emlBody(textBody, htmlBody string) string {
	text := textEntity("text/plain", textBody)
	html := textEntity("text/html", htmlBody)
	switch {
	case textBody != "" && htmlBody != "":
		return multipart("alternative", []string{text, html})
	case textBody != "":
		return text
	case htmlBody != "":
		return html
	}
	return ""
}

func textEntity(mediaType, body string) string {
	var encoded bytes.Buffer
	w := quotedprintable.NewWriter(&encoded)
	_, err := w.Write([]byte(body))
	must(err)
	must(w.Close())
	return "Content-Type: " + mediaType + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + encoded.String()
}

func emlAttachment(a Attachment) string {
	disposition := "attachment"
	if a.Filename != "" {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	}
	if strings.EqualFold(a.ContentType, mimeRFC822) {
		return "Content-Type: message/rfc822\r\nContent-Disposition: " + disposition + "\r\n\r\n" + string(a.Data)
	}
	contentType := a.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if a.Filename != "" {
		if formatted := mime.FormatMediaType(contentType, map[string]string{"name": a.Filename}); formatted != "" {
			contentType = formatted
		}
	}
	return "Content-Type: " + contentType + "\r\nContent-Disposition: " + disposition +
		"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + wrapBase64(a.Data)
}

func wrapBase64(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var out strings.Builder
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded)
	return out.String()
}

// multipart returns a multipart entity (headers, blank line, parts) whose
// boundary derives from the parts, so nested fixtures never share one.
func multipart(subtype string, parts []string) string {
	joined := strings.Join(parts, "\x00")
	sum := sha256.Sum256([]byte(subtype + "\x00" + joined))
	boundary := "=_iterabase_" + hex.EncodeToString(sum[:12])
	if strings.Contains(joined, boundary) {
		panic("admissiontest: MIME boundary collides with part content")
	}
	var out strings.Builder
	out.WriteString("Content-Type: multipart/" + subtype + "; boundary=\"" + boundary + "\"\r\n\r\n")
	for _, part := range parts {
		out.WriteString("--" + boundary + "\r\n" + part + "\r\n")
	}
	out.WriteString("--" + boundary + "--\r\n")
	return out.String()
}

// Outlook .msg fixtures.

// PR_ATTACH_METHOD values for MSGAttachment.Method (MS-OXCMSG 2.2.2.9).
const (
	AttachByValue         = 1
	AttachEmbeddedMessage = 5
	AttachOLE             = 6
)

const (
	msgPropertiesStream  = "__properties_version1.0"
	msgSubstgPrefix      = "__substg1.0_"
	msgAttachPrefix      = "__attach_version1.0_#"
	msgEmbeddedStorage   = "__substg1.0_3701000D"
	msgAttachDataStream  = "__substg1.0_37010102"
	msgTopHeaderBytes    = 32
	msgEmbedHeaderBytes  = 24
	msgAttachHeaderBytes = 8
	msgPropertyBytes     = 16

	propSubject        = 0x0037
	propBody           = 0x1000
	propHTML           = 0x1013
	propAttachLongName = 0x3707
	propAttachMIMETag  = 0x370E
	tagAttachMethod    = 0x37050003
)

// MSGAttachment is one attachment of a BuildMSG fixture. A zero Method means
// AttachByValue; Embedded holds the property streams of an embedded message
// (see MSGEmbeddedMessage) for AttachEmbeddedMessage.
type MSGAttachment struct {
	Filename string
	MIMETag  string
	Data     []byte
	Method   uint32
	Embedded []*CFBNode
}

// BuildMSG returns an Outlook .msg compound file with a unicode PR_SUBJECT,
// a unicode PR_BODY when textBody is non-empty, a binary PR_HTML when
// htmlBody is non-nil, and the given attachments.
func BuildMSG(subject, textBody string, htmlBody []byte, attachments ...MSGAttachment) []byte {
	return CFB(append(msgMessage(msgTopHeaderBytes, subject, textBody, htmlBody), msgAttachmentNodes(attachments)...)...)
}

// msgAttachmentNodes returns one attachment storage per attachment.
func msgAttachmentNodes(attachments []MSGAttachment) []*CFBNode {
	var nodes []*CFBNode
	for i, a := range attachments {
		method := a.Method
		if method == 0 {
			method = AttachByValue
		}
		children := []*CFBNode{
			CFBStream(msgPropertiesStream, msgAttachProperties(method)),
			CFBStream(propertyStream(propAttachLongName, "001F"), utf16LE(a.Filename)),
		}
		if a.MIMETag != "" {
			children = append(children, CFBStream(propertyStream(propAttachMIMETag, "001F"), utf16LE(a.MIMETag)))
		}
		if a.Data != nil {
			children = append(children, CFBStream(msgAttachDataStream, a.Data))
		}
		if a.Embedded != nil {
			children = append(children, CFBStorage(msgEmbeddedStorage, a.Embedded...))
		}
		nodes = append(nodes, CFBStorage(fmt.Sprintf("%s%08X", msgAttachPrefix, i), children...))
	}
	return nodes
}

// MSG returns an Outlook .msg with a unicode PR_BODY and each attachment
// by value (method 1) carrying its long filename, MIME tag, and data.
func MSG(subject, textBody string, attachments ...Attachment) []byte {
	converted := make([]MSGAttachment, 0, len(attachments))
	for _, a := range attachments {
		data := a.Data
		if data == nil {
			data = []byte{}
		}
		converted = append(converted, MSGAttachment{Filename: a.Filename, MIMETag: a.ContentType, Data: data})
	}
	return BuildMSG(subject, textBody, nil, converted...)
}

// MSGWithOLEAttachment returns an Outlook .msg whose only attachment is an
// embedded OLE object (method 6).
func MSGWithOLEAttachment() []byte {
	return BuildMSG("Embedded object", "See the attached object.", nil,
		MSGAttachment{Filename: "object.bin", Data: []byte("ole"), Method: AttachOLE})
}

// MSGEmbeddedMessage returns the property streams of an embedded message,
// for MSGAttachment.Embedded.
func MSGEmbeddedMessage(subject, textBody string) []*CFBNode {
	return msgMessage(msgEmbedHeaderBytes, subject, textBody, nil)
}

// MSGEmbeddedMessageWith returns an embedded message with an optional
// binary PR_HTML and its own attachments, for MSGAttachment.Embedded.
func MSGEmbeddedMessageWith(subject, textBody string, htmlBody []byte, attachments ...MSGAttachment) []*CFBNode {
	return append(msgMessage(msgEmbedHeaderBytes, subject, textBody, htmlBody), msgAttachmentNodes(attachments)...)
}

func msgMessage(headerBytes int, subject, body string, html []byte) []*CFBNode {
	nodes := []*CFBNode{
		CFBStream(msgPropertiesStream, make([]byte, headerBytes)),
		CFBStream(propertyStream(propSubject, "001F"), utf16LE(subject)),
	}
	if body != "" {
		nodes = append(nodes, CFBStream(propertyStream(propBody, "001F"), utf16LE(body)))
	}
	if html != nil {
		nodes = append(nodes, CFBStream(propertyStream(propHTML, "0102"), html))
	}
	return nodes
}

// msgAttachProperties is an attachment property stream holding only
// PR_ATTACH_METHOD.
func msgAttachProperties(method uint32) []byte {
	out := make([]byte, msgAttachHeaderBytes+msgPropertyBytes)
	binary.LittleEndian.PutUint32(out[msgAttachHeaderBytes:], tagAttachMethod)
	binary.LittleEndian.PutUint32(out[msgAttachHeaderBytes+8:], method)
	return out
}

func propertyStream(id uint16, propType string) string {
	return fmt.Sprintf("%s%04X%s", msgSubstgPrefix, id, propType)
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[i*2:], unit)
	}
	return out
}
