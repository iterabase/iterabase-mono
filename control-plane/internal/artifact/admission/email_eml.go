package admission

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
)

// mailLeaf is one decoded non-multipart MIME part.
type mailLeaf struct {
	mediaType   string
	charset     string
	disposition string
	filename    string
	contentID   string
	inRelated   bool
	body        []byte
}

func (l *mailLeaf) isBodyCandidate() bool {
	return (l.mediaType == "text/plain" || l.mediaType == "text/html") &&
		l.disposition != "attachment" && l.filename == ""
}

// evaluateEML admits an RFC 822 message as a container. Beyond
// ExpandableDepth it is accepted unexpanded once its header parses.
func evaluateEML(in Input) Result {
	msg, err := mail.ReadMessage(bytes.NewReader(in.Data))
	if err != nil {
		return reject(ReasonMalformed)
	}
	if in.ContainerDepth > ExpandableDepth {
		return accept(MIMEEML)
	}
	walker := &mimeWalker{budget: newBudget()}
	if reason := walker.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0, false); reason != "" {
		return reject(reason)
	}
	builder := &containerBuilder{}
	if reason := addEMLChildren(builder, walker.leaves); reason != "" {
		return reject(reason)
	}
	return builder.result(MIMEEML)
}

// addEMLChildren selects the body (text/plain preferred, else sanitised
// HTML) and adds every real attachment. Inline images referenced from the
// HTML by Content-ID stay inside the HTML source evidence, not as children.
func addEMLChildren(builder *containerBuilder, leaves []mailLeaf) string {
	var plain, rich *mailLeaf
	var htmlSources [][]byte
	for i := range leaves {
		leaf := &leaves[i]
		if !leaf.isBodyCandidate() {
			continue
		}
		if leaf.mediaType == "text/html" {
			htmlSources = append(htmlSources, bytes.ToLower(leaf.body))
			if rich == nil {
				rich = leaf
			}
		} else if plain == nil && len(bytes.TrimSpace(leaf.body)) > 0 {
			plain = leaf
		}
	}
	switch {
	case plain != nil:
		text, reason := decodeCharset(plain.charset, plain.body)
		if reason != "" {
			return reason
		}
		builder.addBody(text, nil)
	case rich != nil:
		source, reason := decodeCharset(rich.charset, rich.body)
		if reason != "" {
			return reason
		}
		builder.addBody(sanitizeHTML(source), rich.body)
	}
	for i := range leaves {
		leaf := &leaves[i]
		if leaf.isBodyCandidate() || isInlineImage(leaf, htmlSources) {
			continue
		}
		builder.addAttachment(leaf.filename, leaf.mediaType, leaf.body)
	}
	return ""
}

func isInlineImage(leaf *mailLeaf, htmlSources [][]byte) bool {
	if !strings.HasPrefix(leaf.mediaType, "image/") || leaf.contentID == "" {
		return false
	}
	if leaf.disposition == "inline" && leaf.inRelated {
		return true
	}
	reference := []byte("cid:" + strings.ToLower(leaf.contentID))
	for _, source := range htmlSources {
		if bytes.Contains(source, reference) {
			return true
		}
	}
	return false
}

// mimeWalker flattens a MIME tree into decoded leaves within the part,
// recursion, and expanded-byte bounds.
type mimeWalker struct {
	budget *budget
	parts  int
	leaves []mailLeaf
}

func (w *mimeWalker) walk(header textproto.MIMEHeader, body io.Reader, depth int, inRelated bool) string {
	w.parts++
	if w.parts > MaxPackageEntries || depth > MaxRecursion {
		return ReasonResourceLimit
	}
	mediaType, params := "text/plain", map[string]string{}
	if value := header.Get("Content-Type"); value != "" {
		var err error
		mediaType, params, err = mime.ParseMediaType(value)
		if mediaType == "" || (err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter)) {
			return ReasonMalformed
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		return w.walkMultipart(body, params["boundary"], depth, mediaType == "multipart/related")
	}
	decoded, reason := decodeTransferEncoding(header.Get("Content-Transfer-Encoding"), body, w.budget)
	if reason != "" {
		return reason
	}
	disposition, dispositionParams, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	filename := dispositionParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	if decodedName, err := wordDecoder.DecodeHeader(filename); err == nil {
		filename = decodedName
	}
	w.leaves = append(w.leaves, mailLeaf{
		mediaType: mediaType, charset: params["charset"], disposition: disposition,
		filename: filename, contentID: strings.Trim(header.Get("Content-ID"), "<> \t"),
		inRelated: inRelated, body: decoded,
	})
	return ""
}

func (w *mimeWalker) walkMultipart(body io.Reader, boundary string, depth int, related bool) string {
	if boundary == "" {
		return ReasonMalformed
	}
	reader := multipart.NewReader(body, boundary)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ReasonMalformed
		}
		if reason := w.walk(part.Header, part, depth+1, related); reason != "" {
			return reason
		}
	}
}

// decodeTransferEncoding decodes one part body, charging both the raw and
// decoded bytes against the expanded-byte budget.
func decodeTransferEncoding(encoding string, body io.Reader, b *budget) ([]byte, string) {
	reader := &budgetReader{r: body, b: b}
	raw, err := io.ReadAll(reader)
	if reader.exceeded {
		return nil, ReasonResourceLimit
	}
	if err != nil {
		return nil, ReasonMalformed
	}
	var decoded []byte
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return raw, ""
	case "base64":
		decoded, err = decodeBase64(raw)
	case "quoted-printable":
		decoded, err = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	default:
		return nil, ReasonMalformed
	}
	if err != nil {
		return nil, ReasonMalformed
	}
	if !b.take(int64(len(decoded))) {
		return nil, ReasonResourceLimit
	}
	return decoded, ""
}

func decodeBase64(raw []byte) ([]byte, error) {
	clean := bytes.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, raw)
	clean = bytes.TrimRight(clean, "=")
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, err := base64.RawStdEncoding.Decode(out, clean)
	return out[:n], err
}
