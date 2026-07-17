package changelog

import (
	"bytes"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

type Candidate struct {
	URL   *url.URL
	Label string
	Score float64
	Depth int
}

func DiscoverLinks(base *url.URL, content []byte, selectedVersion ...string) []Candidate {
	if base == nil {
		return nil
	}
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return nil
	}
	byURL := make(map[string]Candidate)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			var href string
			for _, a := range n.Attr {
				if a.Key == "href" {
					href = a.Val
					break
				}
			}
			label := strings.Join(nodeText(n), " ")
			label = strings.Join(strings.Fields(label), " ")
			version := ""
			if len(selectedVersion) > 0 {
				version = normalizeVersion(selectedVersion[0])
			}
			score := linkScore(label, href, version)
			if score > 0 {
				if ref, e := url.Parse(href); e == nil {
					u := base.ResolveReference(ref)
					u.Fragment = ""
					if (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
						key := u.String()
						c := Candidate{URL: u, Label: label, Score: score, Depth: 1}
						if old, ok := byURL[key]; !ok || c.Score > old.Score {
							byURL[key] = c
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
	out := make([]Candidate, 0, len(byURL))
	for _, c := range byURL {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].URL.String() < out[j].URL.String()
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func nodeText(n *html.Node) []string {
	var out []string
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			out = append(out, x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func linkScore(label, href, selectedVersion string) float64 {
	s := strings.ToLower(label + " " + href)
	switch {
	case selectedVersion != "" && strings.Contains(normalizeVersion(s), selectedVersion):
		return 1.1
	case strings.Contains(s, "changelog"):
		return 1
	case strings.Contains(s, "release notes"):
		return .9
	case strings.Contains(s, "changes"):
		return .8
	case strings.Contains(s, "release"):
		return .7
	case versionToken.MatchString(s):
		return .6
	case strings.Contains(s, "history"):
		return .5
	default:
		return 0
	}
}
