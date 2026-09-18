// Package webpage makes static homepage content readable in the document inspector.
package webpage

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Markdown preserves document structure without running scripts or fetching assets.
// Prefer an explicit main region, but retain the body on older and sparse sites.
func Markdown(pageURL *url.URL, source []byte) (string, error) {
	if pageURL == nil {
		return "", fmt.Errorf("homepage URL is missing")
	}
	reader, err := charset.NewReader(bytes.NewReader(source), "")
	if err != nil {
		return "", err
	}
	doc, err := html.ParseWithOptions(reader, html.ParseOptionEnableScripting(false))
	if err != nil {
		return "", err
	}
	title, description := "", ""
	linkBase := pageURL
	baseFound := false
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.Data {
		case "title":
			title = strings.Join(strings.Fields(nodeText(n)), " ")
		case "meta":
			if strings.EqualFold(attribute(n, "name"), "description") || (description == "" && strings.EqualFold(attribute(n, "property"), "og:description")) {
				description = attribute(n, "content")
			}
		case "base":
			if !baseFound {
				if resolved := safeLink(pageURL, attribute(n, "href")); resolved != "" {
					candidate, _ := url.Parse(resolved)
					if candidate.Scheme == "http" || candidate.Scheme == "https" {
						linkBase, baseFound = candidate, true
					}
				}
			}
		}
	})
	clean(doc, linkBase)
	content := doc
	var main *html.Node
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "body" {
			content = n
		}
		if main == nil && (n.Data == "main" || attribute(n, "role") == "main") && strings.TrimSpace(nodeText(n)) != "" {
			main = n
		}
	})
	if main != nil {
		content = main
	} else if article, err := readability.FromDocument(doc, linkBase); err == nil && article.Node != nil && article.Length >= 500 {
		// Reader mode helps older pages whose layout has no main landmark.
		// Keep the complete body for short pages rather than discarding useful
		// landing-page links because they don't look like article prose.
		content = article.Node
	}
	// Build a fragment so the converter escapes metadata just like page text.
	fragment := &html.Node{Type: html.ElementNode, Data: "div"}
	hasHeading := false
	walk(content, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "h1" {
			hasHeading = true
		}
	})
	if !hasHeading && title != "" {
		appendTextElement(fragment, "h1", title)
	}
	if strings.TrimSpace(nodeText(content)) == "" {
		if description != "" {
			appendTextElement(fragment, "p", description)
		}
		appendTextElement(fragment, "p", "This page has no readable static content. It may need JavaScript; open the homepage in a browser to read it.")
	}
	if content.Parent != nil {
		content.Parent.RemoveChild(content)
	}
	fragment.AppendChild(content)
	link := &html.Node{Type: html.ElementNode, Data: "a", Attr: []html.Attribute{{Key: "href", Val: pageURL.String()}}}
	link.AppendChild(&html.Node{Type: html.TextNode, Data: "Open homepage in browser"})
	footer := &html.Node{Type: html.ElementNode, Data: "p"}
	footer.AppendChild(link)
	fragment.AppendChild(footer)
	conv := converter.NewConverter(converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))
	conv.Register.TagType("noscript", converter.TagTypeBlock, converter.PriorityEarly)
	markdown, err := conv.ConvertNode(fragment)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(terminalText(string(markdown))), nil
}

func clean(root *html.Node, baseURL *url.URL) {
	for n := root.FirstChild; n != nil; {
		next := n.NextSibling
		if n.Type == html.ElementNode && hidden(n) {
			root.RemoveChild(n)
		} else {
			if n.Type == html.TextNode {
				n.Data = terminalText(n.Data)
			}
			for i := range n.Attr {
				if n.Attr[i].Key == "href" || n.Attr[i].Key == "src" {
					n.Attr[i].Val = safeLink(baseURL, n.Attr[i].Val)
				}
			}
			clean(n, baseURL)
		}
		n = next
	}
}

func hidden(n *html.Node) bool {
	switch n.Data {
	case "head", "script", "style", "template", "svg", "canvas", "iframe", "object", "embed":
		return true
	}
	for _, attr := range n.Attr {
		if attr.Key == "hidden" || (attr.Key == "aria-hidden" && strings.EqualFold(attr.Val, "true")) {
			return true
		}
	}
	style := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, attribute(n, "style"))
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

func safeLink(baseURL *url.URL, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	ref, err := url.Parse(strings.TrimSpace(terminalText(raw)))
	if err != nil {
		return ""
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.User != nil {
		return ""
	}
	switch resolved.Scheme {
	case "http", "https", "mailto":
		return resolved.String()
	default:
		return ""
	}
}

func attribute(n *html.Node, name string) string {
	for _, attr := range n.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func nodeText(n *html.Node) string {
	var text strings.Builder
	walk(n, func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
	})
	return text.String()
}

func appendTextElement(parent *html.Node, tag, text string) {
	n := &html.Node{Type: html.ElementNode, Data: tag}
	n.AppendChild(&html.Node{Type: html.TextNode, Data: terminalText(text)})
	parent.AppendChild(n)
}

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}
