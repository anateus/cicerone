package changelog

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
)

type Extracted struct {
	Title, Text string
	Links       []Candidate
}

type ContentExtractor interface {
	Extract(base *url.URL, html []byte) (Extracted, error)
}

type ReadabilityExtractor struct{}

func (ReadabilityExtractor) Extract(base *url.URL, source []byte) (Extracted, error) {
	if base == nil {
		return Extracted{}, fmt.Errorf("extract base URL is nil")
	}
	doc, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return Extracted{}, err
	}
	pruneBoilerplate(doc)
	article, err := readability.FromDocument(doc, base)
	if err != nil {
		return Extracted{}, err
	}
	text := renderPlain(article.Node)
	if text == "" {
		text = sanitizeText(article.TextContent)
	}
	return Extracted{Title: sanitizeText(article.Title), Text: text, Links: DiscoverLinks(base, source)}, nil
}

func pruneBoilerplate(root *html.Node) {
	for child := root.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && (child.Data == "header" || child.Data == "footer" || child.Data == "nav" || child.Data == "aside") {
			root.RemoveChild(child)
		} else {
			pruneBoilerplate(child)
		}
		child = next
	}
}

func renderPlain(root *html.Node) string {
	if root == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(n *html.Node, listDepth int) {
		if n.Type == html.TextNode {
			b.WriteString(sanitizeText(n.Data))
			return
		}
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, listDepth)
			}
			return
		}
		tag := strings.ToLower(n.Data)
		switch tag {
		case "script", "style", "noscript", "svg":
			return
		case "h1", "h2", "h3", "h4", "h5", "h6":
			blockBreak(&b)
			b.WriteString(strings.Repeat("#", int(tag[1]-'0')))
			b.WriteByte(' ')
		case "li":
			blockBreak(&b)
			b.WriteString(strings.Repeat("  ", listDepth))
			b.WriteString("- ")
		case "p", "div", "article", "section", "blockquote", "pre":
			blockBreak(&b)
		case "br":
			b.WriteByte('\n')
		}
		nextDepth := listDepth
		if tag == "ul" || tag == "ol" {
			nextDepth++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, nextDepth)
		}
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6", "li", "p", "div", "article", "section", "blockquote", "pre":
			blockBreak(&b)
		}
	}
	walk(root, -1)
	lines := strings.Split(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := true
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				out = append(out, "")
				blank = true
			}
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func blockBreak(b *strings.Builder) {
	s := b.String()
	if s == "" {
		return
	}
	if strings.HasSuffix(s, "\n\n") {
		return
	}
	if strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	} else {
		b.WriteString("\n\n")
	}
}

func sanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' {
			return '\n'
		}
		if unicode.IsControl(r) || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
