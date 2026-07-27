package upstream

import (
	"bytes"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

var forgeHosts = map[string]bool{
	"github.com":    true,
	"codeberg.org":  true,
	"gitlab.com":    true,
	"bitbucket.org": true,
}

func CanonicalRepository(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if !forgeHosts[host] {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" || strings.HasPrefix(parts[1], "-") {
		return "", false
	}
	owner, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", false
	}
	repo, err := url.PathUnescape(strings.TrimSuffix(parts[1], ".git"))
	if err != nil || repo == "" {
		return "", false
	}
	return "https://" + host + "/" + owner + "/" + repo, true
}

type repositoryCandidate struct {
	url   string
	score int
}

func DiscoverRepository(base *url.URL, body []byte, packageName string) (string, bool) {
	if base == nil {
		return "", false
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	wantName := normalizedName(packageName)
	var candidates []repositoryCandidate
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attribute(n, "href")
			ref, parseErr := url.Parse(href)
			if parseErr == nil {
				resolved := base.ResolveReference(ref)
				canonical, ok := CanonicalRepository(resolved.String())
				if ok {
					repoName := canonical[strings.LastIndex(canonical, "/")+1:]
					if normalizedName(repoName) == wantName && wantName != "" {
						contextText := strings.ToLower(strings.Join(repositoryLinkContext(n), " "))
						score := repositoryContextScore(contextText)
						if score > 0 {
							candidates = append(candidates, repositoryCandidate{url: canonical, score: score})
						}
					}
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if len(candidates) == 0 {
		return "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].url < candidates[j].url
	})
	if len(candidates) > 1 && candidates[0].score == candidates[1].score && candidates[0].url != candidates[1].url {
		return "", false
	}
	return candidates[0].url, true
}

func repositoryLinkContext(anchor *html.Node) []string {
	result := nodeText(anchor)
	parent := anchor.Parent
	if parent == nil || parent.Type != html.ElementNode {
		return result
	}
	switch parent.Data {
	case "li", "p", "dt", "dd":
		return nodeText(parent)
	default:
		return result
	}
}

func repositoryContextScore(text string) int {
	switch {
	case strings.Contains(text, "repository"):
		return 100
	case strings.Contains(text, "source code") || strings.Contains(text, "source repository"):
		return 90
	case strings.Contains(text, "development"):
		return 80
	case strings.Contains(text, "source") || strings.Contains(text, "contribute"):
		return 60
	default:
		return 0
	}
}

func normalizedName(value string) string {
	value = strings.TrimSuffix(value, ".git")
	if index := strings.LastIndex(value, "/"); index >= 0 {
		value = value[index+1:]
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}

func attribute(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) []string {
	if n == nil {
		return nil
	}
	var result []string
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			result = append(result, current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return result
}
