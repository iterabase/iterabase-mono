package admission

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlSkipped elements contribute no text: scripts, styles, metadata, and
// embedded foreign or active content.
var htmlSkipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Template: true,
	atom.Noscript: true, atom.Title: true, atom.Svg: true, atom.Math: true,
	atom.Object: true, atom.Iframe: true, atom.Noembed: true, atom.Noframes: true,
}

// htmlBlocks start or end a line of text.
var htmlBlocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true, atom.Tr: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Table: true, atom.Ul: true, atom.Ol: true, atom.Blockquote: true, atom.Pre: true,
	atom.Hr: true, atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
}

// sanitizeHTML derives plain UTF-8 text from an untrusted HTML email body.
// Comments, scripts, styles, and metadata are dropped; entities are decoded
// by the tokenizer; block elements become line breaks; whitespace collapses.
// The output is text only and is never rendered as HTML.
func sanitizeHTML(source string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	var text strings.Builder
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return collapseText(text.String())
		case html.TextToken:
			if skipDepth == 0 {
				text.Write(tokenizer.Text())
			}
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			tag := atom.Lookup(name)
			if htmlSkipped[tag] {
				skipDepth++
			} else if htmlBlocks[tag] {
				text.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := atom.Lookup(name)
			if htmlSkipped[tag] && skipDepth > 0 {
				skipDepth--
			} else if htmlBlocks[tag] {
				text.WriteByte('\n')
			}
		case html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			if htmlBlocks[atom.Lookup(name)] {
				text.WriteByte('\n')
			}
		}
	}
}

// collapseText collapses horizontal whitespace runs to one space, trims each
// line, drops control characters, and keeps at most one blank line in a row.
func collapseText(raw string) string {
	var out bytes.Buffer
	blank := 0
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.Join(strings.FieldsFunc(line, isCollapsible), " ")
		if line == "" {
			blank++
			continue
		}
		if out.Len() > 0 {
			out.WriteString(strings.Repeat("\n", min(blank+1, 2)))
		}
		blank = 0
		out.WriteString(line)
	}
	return out.String()
}

func isCollapsible(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\f' || r == '\v' || r == 0xA0 || isControl(r) || r == 0xFEFF
}
