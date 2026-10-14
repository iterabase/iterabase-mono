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

func TestEMLPrefersPlainAlternative(t *testing.T) {
	data := multipartMessage("alternative",
		mimePart("Content-Type: text/plain", "plain text"),
		mimePart("Content-Type: text/html", "<p>html text</p>"),
	)
	got := evaluateEMLAt(data, 1)
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 1)
	assert.Equal(t, "plain text", string(got.Children[0].Data))
	assert.Nil(t, got.Children[0].SourceHTML)
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

func TestMSGEmbeddedMessageBecomesTextEML(t *testing.T) {
	embedded := admissiontest.MSGEmbeddedMessage("Fwd: résumé", "inner body")
	data := admissiontest.BuildMSG("Quarterly report", "outer", nil, admissiontest.MSGAttachment{Filename: "Original.msg", Method: attachEmbeddedMsg, Embedded: embedded})
	got := Evaluate(Input{Data: data, Filename: "mail.msg", ContainerDepth: 1})
	require.True(t, got.Accepted, got.Reason)
	require.Len(t, got.Children, 2)
	child := got.Children[1]
	assert.Equal(t, "Original.eml", child.Filename)
	assert.Equal(t, MIMEEML, child.ClaimedMIME)
	assert.Contains(t, string(child.Data), "Subject: =?utf-8?q?Fwd:_r=C3=A9sum=C3=A9?=\r\n")
	assert.True(t, strings.HasSuffix(string(child.Data), "\r\n\r\ninner body"))
	childResult := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: 2})
	assert.True(t, childResult.Accepted, childResult.Reason)
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
