package web

import (
	"regexp"
	"strings"
)

// Token is a piece of login help: plain text or a link (Href set). Rendered by the template, never as raw HTML.
type Token struct{ Text, Href string }

var (
	// paraSep is a blank line: two or more newlines with only spaces or tabs between them.
	paraSep = regexp.MustCompile(`(?:\r?\n[ \t]*){2,}`)
	// linkRe matches http(s) URLs and bare email addresses.
	linkRe = regexp.MustCompile(`https?://[^\s<>"]+|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// trailingPunct is cut from the end of a matched URL: it usually ends the sentence, not the link.
const trailingPunct = `.,;:!?)]}'"`

// helpParagraphs splits on blank lines; within a paragraph, http(s) URLs and bare email addresses become tokens with
// Href (mailto: for email). Single newlines are kept as "\n" text (CSS white-space: pre-line).
func helpParagraphs(s string) [][]Token {
	var out [][]Token
	for _, p := range paraSep.Split(strings.ReplaceAll(s, "\r\n", "\n"), -1) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var toks []Token
		text := func(t string) {
			if t == "" {
				return
			}
			if n := len(toks); n > 0 && toks[n-1].Href == "" {
				toks[n-1].Text += t
				return
			}
			toks = append(toks, Token{Text: t})
		}
		last := 0
		for _, m := range linkRe.FindAllStringIndex(p, -1) {
			text(p[last:m[0]])
			link, rest := p[m[0]:m[1]], ""
			href := ""
			if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
				trimmed := strings.TrimRight(link, trailingPunct)
				link, rest = trimmed, link[len(trimmed):]
				if link != "http://" && link != "https://" {
					href = link
				}
			} else {
				href = "mailto:" + link
			}
			if href == "" {
				text(link)
			} else {
				toks = append(toks, Token{Text: link, Href: href})
			}
			text(rest)
			last = m[1]
		}
		text(p[last:])
		out = append(out, toks)
	}
	return out
}
