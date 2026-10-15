package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/richardlehane/mscfb"
)

var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

const (
	msgPropertiesStream  = "__properties_version1.0"
	msgSubstgPrefix      = "__substg1.0_"
	msgAttachPrefix      = "__attach_version1.0_#"
	msgEmbeddedStorage   = "__substg1.0_3701000D"
	msgAttachHeaderBytes = 8
	msgPropertyBytes     = 16

	propSubject          = 0x0037
	propSenderEmail      = 0x0C1F
	propBody             = 0x1000
	propHTML             = 0x1013
	propDisplayName      = 0x3001
	propAttachFilename   = 0x3704
	propAttachLongName   = 0x3707
	propAttachMIMETag    = 0x370E
	tagAttachMethod      = 0x37050003
	attachDataBinaryName = "__substg1.0_37010102"
)

// PR_ATTACH_METHOD values (MS-OXCMSG 2.2.2.9).
const (
	attachNone         = 0
	attachByValue      = 1
	attachByReference  = 2
	attachByRefResolve = 3
	attachByRefOnly    = 4
	attachEmbeddedMsg  = 5
	attachOLE          = 6
	attachByWebRef     = 7
)

// cfbTree is a bounded, read-once view of a compound file's streams.
type cfbTree struct {
	size     int64
	budget   *budget
	streams  map[string]*mscfb.File
	names    map[string]bool
	attaches map[string][]string // attachment storages by parent storage
	cache    map[string][]byte
}

// openCFB parses the compound file directory. mscfb is third-party parsing
// of hostile bytes, so a panic is converted to a malformed rejection.
func openCFB(data []byte) (tree *cfbTree, reason string) {
	defer func() {
		if recover() != nil {
			tree, reason = nil, ReasonMalformed
		}
	}()
	reader, err := mscfb.New(bytes.NewReader(data))
	if err != nil {
		return nil, ReasonMalformed
	}
	if len(reader.File) > MaxPackageEntries {
		return nil, ReasonResourceLimit
	}
	tree = &cfbTree{
		size: int64(len(data)), budget: newBudget(), streams: map[string]*mscfb.File{},
		names: map[string]bool{}, attaches: map[string][]string{}, cache: map[string][]byte{},
	}
	for _, f := range reader.File {
		tree.names[f.Name] = true
		key := strings.Join(append(append([]string{}, f.Path...), f.Name), "/")
		switch {
		case !f.FileInfo().IsDir():
			tree.streams[key] = f
		case strings.HasPrefix(f.Name, msgAttachPrefix):
			parent := strings.Join(f.Path, "/")
			tree.attaches[parent] = append(tree.attaches[parent], key)
		}
	}
	for _, storages := range tree.attaches {
		sort.Strings(storages)
	}
	return tree, ""
}

// detectCFB distinguishes an Outlook message from encrypted OOXML and the
// legacy Office formats, which are outside the allow-list.
func detectCFB(data []byte) (string, string) {
	tree, reason := openCFB(data)
	if reason != "" {
		return "", reason
	}
	if tree.names["EncryptedPackage"] || tree.names["EncryptionInfo"] {
		return "", ReasonEncrypted
	}
	if tree.streams[msgPropertiesStream] != nil && tree.hasTopLevelProperty() {
		return MIMEMSG, ""
	}
	return "", ReasonUnsupportedType
}

func (t *cfbTree) hasTopLevelProperty() bool {
	for key := range t.streams {
		if strings.HasPrefix(key, msgSubstgPrefix) {
			return true
		}
	}
	return false
}

// evaluateMSG admits an Outlook message as a container, expanding the body
// and by-value attachments. Embedded OLE objects reject the container.
func evaluateMSG(in Input) (result Result) {
	tree, reason := openCFB(in.Data)
	if reason != "" {
		return reject(reason)
	}
	if in.ContainerDepth > ExpandableDepth {
		return accept(MIMEMSG)
	}
	defer func() {
		if recover() != nil {
			result = reject(ReasonMalformed)
		}
	}()
	builder := &containerBuilder{}
	text, source, reason := tree.messageBody("")
	if reason != "" {
		return reject(reason)
	}
	builder.addBody(text, source)
	for _, storage := range tree.attaches[""] {
		name, claimed, data, present, reason := tree.attachment(storage, 0)
		if reason != "" {
			return reject(reason)
		}
		if present {
			builder.addAttachment(name, claimed, data)
		}
		if len(builder.children) > MaxChildren {
			return reject(ReasonContainerTooManyParts)
		}
	}
	return builder.result(MIMEMSG)
}

// attachment resolves one attachment storage of a message rendered at depth
// (0 for the top-level message) to the bytes it carries. present is false for
// a reference, which carries no bytes inside the container. An embedded
// message becomes a complete MIME message with its own attachments.
func (t *cfbTree) attachment(storage string, depth int) (name, claimed string, data []byte, present bool, reason string) {
	method, reason := t.attachMethod(storage)
	if reason != "" {
		return "", "", nil, false, reason
	}
	switch method {
	case attachNone, attachByReference, attachByRefResolve, attachByRefOnly, attachByWebRef:
		return "", "", nil, false, ""
	case attachOLE:
		return "", "", nil, false, ReasonEmbeddedObject
	case attachEmbeddedMsg:
		data, reason := t.renderEmbeddedMessage(storage+"/"+msgEmbeddedStorage, depth+1)
		return t.embeddedMessageName(storage), MIMEEML, data, reason == "", reason
	case attachByValue:
		data, reason := t.read(storage + "/" + attachDataBinaryName)
		if reason != "" {
			return "", "", nil, false, reason
		}
		name, reason := t.attachmentName(storage)
		if reason != "" {
			return "", "", nil, false, reason
		}
		claimed, reason := t.stringProperty(storage, propAttachMIMETag)
		return name, claimed, data, reason == "", reason
	}
	return "", "", nil, false, ReasonMalformed
}

func (t *cfbTree) attachMethod(storage string) (uint32, string) {
	props, reason := t.read(storage + "/" + msgPropertiesStream)
	if reason != "" {
		return 0, reason
	}
	for offset := msgAttachHeaderBytes; offset+msgPropertyBytes <= len(props); offset += msgPropertyBytes {
		if binary.LittleEndian.Uint32(props[offset:]) == tagAttachMethod {
			return binary.LittleEndian.Uint32(props[offset+8:]), ""
		}
	}
	return attachByValue, ""
}

func (t *cfbTree) attachmentName(storage string) (string, string) {
	for _, id := range []uint16{propAttachLongName, propAttachFilename} {
		name, reason := t.stringProperty(storage, id)
		if reason != "" || name != "" {
			return name, reason
		}
	}
	return "", ""
}

// embeddedMessageName derives "<name>.eml" for an embedded message.
func (t *cfbTree) embeddedMessageName(storage string) string {
	name, _ := t.attachmentName(storage)
	if name == "" {
		name, _ = t.stringProperty(storage, propDisplayName)
	}
	if ext := extensionOf(name); ext == "msg" || ext == "eml" {
		name = name[:len(name)-len(ext)-1]
	}
	if strings.TrimSpace(name) == "" {
		return AttachedMessageFilename
	}
	return name + ".eml"
}

// messageBody returns the plain body, or text sanitised from the HTML body.
// The original HTML, when present, is always returned as source evidence,
// including when the plain body is preferred. An RTF-only body yields no text.
func (t *cfbTree) messageBody(prefix string) (text string, sourceHTML []byte, reason string) {
	plain, reason := t.stringProperty(prefix, propBody)
	if reason != "" {
		return "", nil, reason
	}
	raw, html, reason := t.htmlBody(prefix)
	if reason != "" {
		return "", nil, reason
	}
	if strings.TrimSpace(plain) != "" {
		return plain, raw, ""
	}
	if raw == nil {
		return "", nil, ""
	}
	return sanitizeHTML(html), raw, ""
}

// htmlBody returns PR_HTML as its original bytes and as decoded text.
func (t *cfbTree) htmlBody(prefix string) (raw []byte, html string, reason string) {
	raw, reason = t.read(join(prefix, propertyStream(propHTML, "0102")))
	if reason != "" {
		return nil, "", reason
	}
	if len(raw) > 0 {
		return raw, decodeLegacyString(raw), ""
	}
	html, reason = t.stringProperty(prefix, propHTML)
	if reason != "" || html == "" {
		return nil, "", reason
	}
	return []byte(html), html, ""
}

// renderEmbeddedMessage renders an embedded message (attach method 5) as a
// complete RFC 5322 message: From, Subject, the plain or sanitised body with
// the original HTML alongside it, and every attachment the embedded message
// carries, recursively. The child keeps its attachments whether or not it is
// expanded: beyond ExpandableDepth they stay inside its quarantined bytes.
// Bodies and attachments are base64, so the message is 7-bit and a nested
// message nests verbatim as message/rfc822.
func (t *cfbTree) renderEmbeddedMessage(prefix string, depth int) ([]byte, string) {
	if depth > MaxRecursion {
		return nil, ReasonResourceLimit
	}
	subject, reason := t.stringProperty(prefix, propSubject)
	if reason != "" {
		return nil, reason
	}
	from, reason := t.stringProperty(prefix, propSenderEmail)
	if reason != "" {
		return nil, reason
	}
	body, _, reason := t.messageBody(prefix)
	if reason != "" {
		return nil, reason
	}
	_, html, reason := t.htmlBody(prefix)
	if reason != "" {
		return nil, reason
	}
	var out bytes.Buffer
	writeHeader(&out, "From", from)
	writeHeader(&out, "Subject", subject)
	mixed := multipart.NewWriter(&out)
	if err := mixed.SetBoundary(embeddedBoundary("mixed", prefix, depth)); err != nil {
		return nil, ReasonMalformed
	}
	fmt.Fprintf(&out, "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed.Boundary())
	if err := writeEmbeddedBody(mixed, prefix, depth, body, html); err != nil {
		return nil, ReasonMalformed
	}
	for _, storage := range t.attaches[prefix] {
		name, claimed, data, present, reason := t.attachment(storage, depth)
		if reason != "" {
			return nil, reason
		}
		if !present {
			continue
		}
		if err := writeEmbeddedAttachment(mixed, name, claimed, data); err != nil {
			return nil, ReasonMalformed
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, ReasonMalformed
	}
	return out.Bytes(), ""
}

// embeddedBoundary is unique per storage and nesting level, and cannot occur
// inside base64 content or a nested message's own boundaries.
func embeddedBoundary(kind, prefix string, depth int) string {
	sum := sha256.Sum256([]byte(prefix))
	return fmt.Sprintf("iterabase-%s-%d-%x", kind, depth, sum[:8])
}

func writeEmbeddedBody(mixed *multipart.Writer, prefix string, depth int, text, html string) error {
	if html == "" {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return writeBase64Part(mixed, textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}}, []byte(text))
	}
	alternative := embeddedBoundary("alternative", prefix, depth)
	part, err := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {fmt.Sprintf("multipart/alternative; boundary=%q", alternative)}})
	if err != nil {
		return err
	}
	inner := multipart.NewWriter(part)
	if err := inner.SetBoundary(alternative); err != nil {
		return err
	}
	if err := writeBase64Part(inner, textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}}, []byte(text)); err != nil {
		return err
	}
	if err := writeBase64Part(inner, textproto.MIMEHeader{"Content-Type": {"text/html; charset=utf-8"}}, []byte(html)); err != nil {
		return err
	}
	return inner.Close()
}

func writeEmbeddedAttachment(mixed *multipart.Writer, name, claimed string, data []byte) error {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disposition == "" {
		disposition = "attachment"
	}
	if claimed == MIMEEML {
		part, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type": {MIMEEML}, "Content-Disposition": {disposition}, "Content-Transfer-Encoding": {"7bit"},
		})
		if err != nil {
			return err
		}
		_, err = part.Write(data)
		return err
	}
	contentType := mime.FormatMediaType(normalizeMIME(claimed), nil)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return writeBase64Part(mixed, textproto.MIMEHeader{"Content-Type": {contentType}, "Content-Disposition": {disposition}}, data)
}

func writeBase64Part(w *multipart.Writer, header textproto.MIMEHeader, data []byte) error {
	header.Set("Content-Transfer-Encoding", "base64")
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 76 {
		if _, err := io.WriteString(part, encoded[:76]+"\r\n"); err != nil {
			return err
		}
		encoded = encoded[76:]
	}
	_, err = io.WriteString(part, encoded+"\r\n")
	return err
}

func writeHeader(out *bytes.Buffer, name, value string) {
	value = strings.Join(strings.FieldsFunc(value, isControl), " ")
	if value == "" {
		return
	}
	fmt.Fprintf(out, "%s: %s\r\n", name, mime.QEncoding.Encode("utf-8", value))
}

// stringProperty reads a PT_UNICODE (001F) or PT_STRING8 (001E) property.
func (t *cfbTree) stringProperty(prefix string, id uint16) (string, string) {
	unicode, reason := t.read(join(prefix, propertyStream(id, "001F")))
	if reason != "" {
		return "", reason
	}
	if len(unicode) > 0 {
		units := make([]uint16, len(unicode)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(unicode[i*2:])
		}
		return strings.TrimRight(string(utf16.Decode(units)), "\x00"), ""
	}
	legacy, reason := t.read(join(prefix, propertyStream(id, "001E")))
	if reason != "" {
		return "", reason
	}
	return strings.TrimRight(decodeLegacyString(legacy), "\x00"), ""
}

// read returns a stream's bytes once, charged against the budget. A missing
// stream reads as empty.
func (t *cfbTree) read(key string) ([]byte, string) {
	if data, ok := t.cache[key]; ok {
		return data, ""
	}
	f := t.streams[key]
	if f == nil {
		return nil, ""
	}
	if f.Size > t.size {
		return nil, ReasonMalformed
	}
	if !t.budget.take(f.Size) {
		return nil, ReasonResourceLimit
	}
	data := make([]byte, f.Size)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, ReasonMalformed
	}
	t.cache[key] = data
	return data, ""
}

func propertyStream(id uint16, propType string) string {
	return fmt.Sprintf("%s%04X%s", msgSubstgPrefix, id, propType)
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}
