package tui

import (
	"crypto/sha256"
	"regexp"
	"strings"
	"sync"

	"charm.land/glamour/v2"
)

var markdownImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)

type markdownCacheKey struct {
	digest [32]byte
	width  int
	light  bool
}

// View can run repeatedly while unrelated progress messages arrive. Cache
// Glamour output by content, width, and theme so a large document is rendered
// once per presentation shape instead of once per frame.
var markdownCache = struct {
	sync.Mutex
	items map[markdownCacheKey]string
}{items: make(map[markdownCacheKey]string)}

const markdownCacheLimit = 8

func renderMarkdown(source string, width int, light bool) string {
	if width < 12 {
		return source
	}
	style := "dark"
	if light {
		style = "light"
	}
	source = markdownImage.ReplaceAllStringFunc(source, func(image string) string {
		match := markdownImage.FindStringSubmatch(image)
		label := strings.TrimSpace(match[1])
		if label == "" {
			label = "Image"
		}
		return "▧ " + label
	})
	key := markdownCacheKey{digest: sha256.Sum256([]byte(source)), width: width, light: light}
	markdownCache.Lock()
	if rendered, ok := markdownCache.items[key]; ok {
		markdownCache.Unlock()
		return rendered
	}
	markdownCache.Unlock()
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width-2),
	)
	if err != nil {
		return source
	}
	rendered, err := renderer.Render(source)
	if err != nil {
		return source
	}
	rendered = strings.TrimSpace(rendered)
	markdownCache.Lock()
	if len(markdownCache.items) >= markdownCacheLimit {
		// Best-effort bounded eviction; exact order is not material.
		for cachedKey := range markdownCache.items {
			delete(markdownCache.items, cachedKey)
			break
		}
	}
	markdownCache.items[key] = rendered
	markdownCache.Unlock()
	return rendered
}
