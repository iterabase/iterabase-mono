package admission

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

const boundary = "BOUNDARY"

func mimePart(headers, body string) string {
	return "--" + boundary + "\r\n" + headers + "\r\n\r\n" + body + "\r\n"
}

func multipartMessage(kind string, parts ...string) []byte {
	return []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: Test\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/" + kind + "; boundary=" + boundary + "\r\n\r\n" +
		strings.Join(parts, "") + "--" + boundary + "--\r\n")
}

func pdfAttachmentPart(t testing.TB, filename string) string {
	return mimePart("Content-Type: application/pdf; name=\"x.pdf\"\r\nContent-Disposition: attachment; filename=\""+filename+"\"\r\nContent-Transfer-Encoding: base64",
		base64.StdEncoding.EncodeToString(admissiontest.PDF()))
}

func evaluateEMLAt(data []byte, depth int) Result {
	return Evaluate(Input{Data: data, Filename: "mail.eml", ClaimedMIME: MIMEEML, ContainerDepth: depth})
}

func TestEMLExpandsBodyAttachmentAndAttachedMessage(t *testing.T) {
	inner := "Subject: inner\r\n\r\ninner body\r\n"
	data := multipartMessage("mixed",
		mimePart("Content-Type: text/plain; charset=utf-8", "Hello body"),
		pdfAttachmentPart(t, "report.pdf"),
		mimePart("Content-Type: message/rfc822", inner),
	)
	for _, depth := range []int{0, 1} {
		got := evaluateEMLAt(data, depth)
		require.True(t, got.Accepted, got.Reason)
		require.Len(t, got.Children, 3)
		assert.Equal(t, Child{Kind: ChildBody, Index: 0, Filename: BodyFilename, ClaimedMIME: MIMETXT, Data: []byte("Hello body")}, got.Children[0])
		assert.Equal(t, Child{Kind: ChildAttachment, Index: 1, Filename: "report.pdf", ClaimedMIME: MIMEPDF, Data: admissiontest.PDF()}, got.Children[1])
		assert.Equal(t, Child{Kind: ChildAttachment, Index: 2, Filename: AttachedMessageFilename, ClaimedMIME: MIMEEML, Data: []byte(inner)}, got.Children[2])
	}
	// Each child is admitted on its own one level deeper; the attached message
	// at depth 2 is accepted unexpanded.
	got := evaluateEMLAt(data, 1)
	for _, child := range got.Children {
		childResult := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: 2})
		assert.True(t, childResult.Accepted, "%s: %s", child.Filename, childResult.Reason)
		assert.Empty(t, childResult.Children)
	}
}

func TestEMLAtDepthTwoIsNotExpanded(t *testing.T) {
	got := evaluateEMLAt(multipartMessage("mixed", pdfAttachmentPart(t, "a.pdf")), 2)
	assert.Equal(t, Result{Accepted: true, CanonicalMIME: MIMEEML}, got)
}

func TestEMLHTMLOnlyBodyIsSanitised(t *testing.T) {
	html := `<html><head><title>T</title><style>p{color:red}</style><script>alert("secret")</script></head>` +
		`<body><p>Hello&nbsp;<b>World</b> &amp; co</p><!-- hidden comment --><div>Second   line</div>` +
		`<img src="cid:logo@example"></body></html>`
	data := multipartMessage("related",
		mimePart("Content-Type: text/html; charset=utf-8", html),
		mimePart("Content-Type: image/png\r\nContent-ID: <logo@example>\r\nContent-Transfer-Encoding: base64",
			base64.StdEncoding.EncodeToString(admissiontest.PNG())),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 1, "the cid-referenced inline image is not a child")
	body := got.Children[0]
	assert.Equal(t, ChildBody, body.Kind)
	assert.Equal(t, "Hello World & co\n\nSecond line", string(body.Data))
	assert.NotContains(t, string(body.Data), "secret")
	assert.NotContains(t, string(body.Data), "hidden")
	assert.NotContains(t, string(body.Data), "color")
	assert.Equal(t, []byte(html), body.SourceHTML)
}

func TestEMLInlineCIDImageInMixedIsExcludedButOtherImagesKept(t *testing.T) {
	data := multipartMessage("mixed",
		mimePart("Content-Type: text/html", `<p>see <img src="CID:pic1"></p>`),
		mimePart("Content-Type: image/png; name=pic.png\r\nContent-ID: <pic1>\r\nContent-Transfer-Encoding: base64",
			base64.StdEncoding.EncodeToString(admissiontest.PNG())),
		mimePart("Content-Type: image/png; name=photo.png\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64",
			base64.StdEncoding.EncodeToString(admissiontest.PNG())),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 2)
	assert.Equal(t, "see", string(got.Children[0].Data))
	assert.Equal(t, "photo.png", got.Children[1].Filename)
}

// The plain alternative is the body; the original HTML is still kept as
// quarantined source evidence (HOR-461 design decision).
func TestEMLPrefersPlainAlternative(t *testing.T) {
	data := multipartMessage("alternative",
		mimePart("Content-Type: text/plain", "plain text"),
		mimePart("Content-Type: text/html", "<p>html text</p>"),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 1)
	assert.Equal(t, "plain text", string(got.Children[0].Data))
	assert.Equal(t, []byte("<p>html text</p>"), got.Children[0].SourceHTML)
}

func TestMSGPlainBodyKeepsHTMLSource(t *testing.T) {
	html := []byte("<p>html text</p>")
	got := Evaluate(Input{Data: admissiontest.BuildMSG("Quarterly report", "plain text", html), Filename: "mail.msg", ContainerDepth: 1})
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 1)
	assert.Equal(t, "plain text", string(got.Children[0].Data))
	assert.Equal(t, html, got.Children[0].SourceHTML)
}

func TestEMLCharsetsAndEncodedWords(t *testing.T) {
	data := multipartMessage("mixed",
		mimePart("Content-Type: text/plain; charset=windows-1252\r\nContent-Transfer-Encoding: quoted-printable", "caf=E9 =80"),
		mimePart("Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"=?UTF-8?B?w6l0w6kucGRm?=\"\r\nContent-Transfer-Encoding: base64",
			base64.StdEncoding.EncodeToString(admissiontest.PDF())),
		mimePart("Content-Type: application/pdf; name=\"=?iso-8859-1?Q?r=E9sum=E9.pdf?=\"\r\nContent-Transfer-Encoding: base64",
			base64.StdEncoding.EncodeToString(admissiontest.PDF())),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 3)
	assert.Equal(t, "café €", string(got.Children[0].Data))
	assert.Equal(t, "été.pdf", got.Children[1].Filename)
	assert.Equal(t, "résumé.pdf", got.Children[2].Filename)
}

func TestEMLChildFilenames(t *testing.T) {
	data := multipartMessage("mixed",
		pdfAttachmentPart(t, "invoice.pdf.exe"),
		pdfAttachmentPart(t, "../../secret‮.pdf"),
		mimePart("Content-Type: application/octet-stream\r\nContent-Disposition: attachment", "raw"),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 3)
	assert.Equal(t, "attachment-0.pdf", got.Children[0].Filename)
	assert.Equal(t, "attachment-1.pdf", got.Children[1].Filename)
	assert.Equal(t, "attachment-2", got.Children[2].Filename)
}

func TestEMLRejections(t *testing.T) {
	tooMany := make([]string, 0, MaxChildren+1)
	for i := range MaxChildren + 1 {
		tooMany = append(tooMany, pdfAttachmentPart(t, fmt.Sprintf("f%d.pdf", i)))
	}
	deep := "Content-Type: text/plain\r\n\r\nleaf"
	for i := range MaxRecursion + 1 {
		b := fmt.Sprintf("b%d", i)
		deep = "Content-Type: multipart/mixed; boundary=" + b + "\r\n\r\n--" + b + "\r\n" + deep + "\r\n--" + b + "--"
	}
	cases := map[string]struct {
		data   []byte
		reason string
	}{
		"too many parts":   {multipartMessage("mixed", tooMany...), ReasonContainerTooManyParts},
		"unknown charset":  {multipartMessage("mixed", mimePart("Content-Type: text/plain; charset=x-unknown-cs", "hi")), ReasonMalformed},
		"bad base64":       {multipartMessage("mixed", mimePart("Content-Type: application/pdf\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64", "!!!!")), ReasonMalformed},
		"unknown encoding": {multipartMessage("mixed", mimePart("Content-Type: application/pdf\r\nContent-Transfer-Encoding: x-uuencode", "x")), ReasonMalformed},
		"missing boundary": {[]byte("Subject: x\r\nContent-Type: multipart/mixed\r\n\r\nbody"), ReasonMalformed},
		"invalid utf-8":    {[]byte("Subject: x\r\nContent-Type: text/plain; charset=utf-8\r\n\r\ncaf\xe9"), ReasonInvalidTextEncoding},
		"too deep":         {[]byte("Subject: deep\r\n" + deep), ReasonResourceLimit},
		"not a message":    {[]byte("no header separator here"), ReasonMalformed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, reject(tc.reason), evaluateEMLAt(tc.data, 1))
		})
	}
}

func TestMSGExpandsBodyAndAttachment(t *testing.T) {
	data := admissiontest.BuildMSG("Quarterly report", "Hello from Outlook", nil, admissiontest.MSGAttachment{Filename: "report.pdf", MIMETag: MIMEPDF, Data: admissiontest.PDF()})
	got := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 1})
	require.True(t, got.Accepted, got.Reason)
	assert.Equal(t, MIMEMSG, got.CanonicalMIME)
	require.Len(t, got.Children, 2)
	assert.Equal(t, Child{Kind: ChildBody, Index: 0, Filename: BodyFilename, ClaimedMIME: MIMETXT, Data: []byte("Hello from Outlook")}, got.Children[0])
	assert.Equal(t, Child{Kind: ChildAttachment, Index: 1, Filename: "report.pdf", ClaimedMIME: MIMEPDF, Data: admissiontest.PDF()}, got.Children[1])

	unexpanded := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 2})
	assert.Equal(t, Result{Accepted: true, CanonicalMIME: MIMEMSG}, unexpanded)
}

func TestMSGHTMLOnlyBody(t *testing.T) {
	html := []byte("<html><body><script>steal()</script><p>Hi&amp;bye</p></body></html>")
	got := Evaluate(Input{Data: admissiontest.BuildMSG("Quarterly report", "", html), Filename: "mail.msg", ContainerDepth: 1})
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 1)
	assert.Equal(t, "Hi&bye", string(got.Children[0].Data))
	assert.Equal(t, html, got.Children[0].SourceHTML)
}

func TestMSGEmbeddedMessageBecomesEML(t *testing.T) {
	embedded := admissiontest.MSGEmbeddedMessage("Fwd: résumé", "inner body")
	data := admissiontest.BuildMSG("Quarterly report", "outer", nil, admissiontest.MSGAttachment{Filename: "Original.msg", Method: attachEmbeddedMsg, Embedded: embedded})
	got := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 1})
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 2)
	child := got.Children[1]
	assert.Equal(t, "Original.eml", child.Filename)
	assert.Equal(t, MIMEEML, child.ClaimedMIME)
	assert.Contains(t, string(child.Data), "Subject: =?utf-8?q?Fwd:_r=C3=A9sum=C3=A9?=\r\n")
	childResult := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: 2})
	assert.True(t, childResult.Accepted, childResult.Reason)
	expanded := evaluateEMLAt(child.Data, 1)
	require.True(t, expanded.Accepted, expanded.Reason)
	require.Len(t, expanded.Children, 1)
	assert.Equal(t, "inner body", string(expanded.Children[0].Data))
}

// Ticket acceptance with native Outlook messages: a received (depth 0) .msg
// holding an embedded message with a PDF yields the body and the embedded
// message; that message, admitted at depth 1, yields its own body and PDF
// (DES-HOR-461-03). Beyond the depth limit the embedded child is accepted
// unexpanded but still carries its PDF, its HTML, and its nested message.
func TestMSGEmbeddedMessageKeepsItsAttachments(t *testing.T) {
	html := []byte("<p>inner <b>html</b></p>")
	nested := admissiontest.MSGEmbeddedMessageWith("Older thread", "older body", nil)
	embedded := admissiontest.MSGEmbeddedMessageWith("Fwd: report", "inner body", html,
		admissiontest.MSGAttachment{Filename: "inner.pdf", MIMETag: MIMEPDF, Data: admissiontest.PDF()},
		admissiontest.MSGAttachment{Filename: "thread.msg", Method: attachEmbeddedMsg, Embedded: nested},
		admissiontest.MSGAttachment{Filename: "link", Method: attachByReference})
	data := admissiontest.BuildMSG("Quarterly report", "outer", nil,
		admissiontest.MSGAttachment{Filename: "Fwd report.msg", Method: attachEmbeddedMsg, Embedded: embedded})

	received := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 0})
	require.True(t, received.Accepted, received.Reason)
	require.Len(t, received.Children, 2)
	assert.Equal(t, "outer", string(received.Children[0].Data))
	child := received.Children[1]
	assert.Equal(t, "Fwd report.eml", child.Filename)
	assert.Equal(t, MIMEEML, child.ClaimedMIME)

	expanded := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: 1})
	require.True(t, expanded.Accepted, expanded.Reason)
	require.Len(t, expanded.Children, 3)
	assert.Equal(t, "inner body", string(expanded.Children[0].Data))
	assert.Equal(t, html, expanded.Children[0].SourceHTML)
	assert.Equal(t, "inner.pdf", expanded.Children[1].Filename)
	assert.Equal(t, admissiontest.PDF(), expanded.Children[1].Data)
	pdf := Evaluate(Input{Data: expanded.Children[1].Data, Filename: "inner.pdf", ClaimedMIME: MIMEPDF, ContainerDepth: 2})
	assert.True(t, pdf.Accepted, pdf.Reason)
	assert.Equal(t, "thread.eml", expanded.Children[2].Filename)
	assert.Equal(t, MIMEEML, expanded.Children[2].ClaimedMIME)
	thread := evaluateEMLAt(expanded.Children[2].Data, 1)
	require.True(t, thread.Accepted, thread.Reason)
	require.Len(t, thread.Children, 1)
	assert.Equal(t, "older body", string(thread.Children[0].Data))

	unexpanded := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: 2})
	assert.True(t, unexpanded.Accepted, unexpanded.Reason)
	assert.Empty(t, unexpanded.Children)
	assert.Contains(t, string(child.Data), "filename=inner.pdf")
}

func TestMSGEmbeddedMessageRejectsNestedOLE(t *testing.T) {
	embedded := admissiontest.MSGEmbeddedMessageWith("Fwd", "inner", nil,
		admissiontest.MSGAttachment{Filename: "obj", Method: attachOLE, Data: []byte("ole")})
	data := admissiontest.BuildMSG("Quarterly report", "outer", nil,
		admissiontest.MSGAttachment{Filename: "Fwd.msg", Method: attachEmbeddedMsg, Embedded: embedded})
	got := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 0})
	assert.False(t, got.Accepted)
	assert.Equal(t, ReasonEmbeddedObject, got.Reason)
}

func TestMSGRejections(t *testing.T) {
	tooMany := make([]admissiontest.MSGAttachment, 0, MaxChildren+1)
	for i := range MaxChildren + 1 {
		tooMany = append(tooMany, admissiontest.MSGAttachment{Filename: fmt.Sprintf("f%d.txt", i), Data: []byte("x")})
	}
	cases := map[string]struct {
		data   []byte
		reason string
	}{
		"ole attachment": {admissiontest.BuildMSG("Quarterly report", "body", nil, admissiontest.MSGAttachment{Filename: "obj", Method: attachOLE, Data: []byte("ole")}), ReasonEmbeddedObject},
		"too many parts": {admissiontest.BuildMSG("Quarterly report", "", nil, tooMany...), ReasonContainerTooManyParts},
		"bad method":     {admissiontest.BuildMSG("Quarterly report", "body", nil, admissiontest.MSGAttachment{Filename: "x", Method: 99}), ReasonMalformed},
		"truncated":      {admissiontest.BuildMSG("Quarterly report", "body", nil)[:1024], ReasonMalformed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, reject(tc.reason), Evaluate(Input{Data: tc.data, Filename: "mail.msg", ContainerDepth: 1}))
		})
	}
}

func TestSanitizeHTML(t *testing.T) {
	cases := map[string]string{
		"<p>a</p><p>b</p>":                                  "a\n\nb",
		"x<br>y":                                            "x\ny",
		"<ul><li>one</li><li>two</li></ul>":                 "one\n\ntwo",
		"<template><p>no</p></template>yes":                 "yes",
		"<noscript>n</noscript><svg><text>s</text></svg>ok": "ok",
		"a  \t b":        "a b",
		"&lt;script&gt;": "<script>",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeHTML(in), in)
	}
}
