package changelog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Artifact is one immutable fetched changelog input.
type Artifact struct {
	ID, URL, MediaType, ETag, LastModified, Hash string
	Raw, Extracted                               []byte
	FetchedAt                                    time.Time
	ParentID                                     *string
}

// Section is the best version-specific portion of an artifact.
type Section struct {
	ArtifactID, Version, Body string
	Confidence                float64
	SourceURL                 string
}

type ReleasePage struct {
	Sections []Section
	NextPage int
}

var (
	atxHeading    = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	setextHeading = regexp.MustCompile(`^\s*(?:=+|-+)\s*$`)
	versionToken  = regexp.MustCompile(`(?i)(?:^|[^0-9])v?([0-9]+(?:\.[0-9A-Za-z-]+)+)(?:$|[^0-9A-Za-z.-])`)
	rangeToken    = regexp.MustCompile(`(?i)v?([0-9]+(?:\.[0-9A-Za-z-]+)+)\s*(?:\.\.|-|–|—|to)\s*v?([0-9]+(?:\.[0-9A-Za-z-]+)+)`)
)

type heading struct {
	title                 string
	start, bodyStart, end int
}

// MatchVersion deterministically selects the strongest version section.
func MatchVersion(version string, artifacts []Artifact) (Section, bool) {
	target := normalizeVersion(version)
	best := Section{}
	found := false
	for _, artifact := range artifacts {
		body := artifact.Extracted
		if len(body) == 0 {
			body = artifact.Raw
		}
		text := string(body)
		headings := markdownHeadings(text)
		for _, h := range headings {
			score := headingScore(target, h.title)
			if score == 0 {
				continue
			}
			candidate := Section{ArtifactID: artifact.ID, Version: version, Body: strings.TrimSpace(text[h.start:h.end]), Confidence: score, SourceURL: artifact.URL}
			if !found || candidate.Confidence > best.Confidence {
				best, found = candidate, true
			}
		}
		if strings.Contains(strings.ToLower(text), strings.ToLower(version)) {
			candidate := Section{ArtifactID: artifact.ID, Version: version, Body: strings.TrimSpace(text), Confidence: .4, SourceURL: artifact.URL}
			if !found || candidate.Confidence > best.Confidence {
				best, found = candidate, true
			}
		}
	}
	return best, found
}

func markdownHeadings(text string) []heading {
	lines := strings.SplitAfter(text, "\n")
	type raw struct {
		title            string
		start, bodyStart int
	}
	var found []raw
	offset := 0
	for i, line := range lines {
		plain := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if match := atxHeading.FindStringSubmatch(plain); match != nil {
			found = append(found, raw{strings.TrimSpace(match[1]), offset, offset + len(line)})
		} else if i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if plain != "" && setextHeading.MatchString(next) {
				found = append(found, raw{strings.TrimSpace(plain), offset, offset + len(line) + len(lines[i+1])})
			}
		}
		offset += len(line)
	}
	result := make([]heading, len(found))
	for i, h := range found {
		end := len(text)
		if i+1 < len(found) {
			end = found[i+1].start
		}
		result[i] = heading{h.title, h.start, h.bodyStart, end}
	}
	return result
}

func headingScore(target, title string) float64 {
	lower := strings.ToLower(strings.TrimSpace(title))
	if strings.Contains(lower, "unreleased") {
		return 0
	}
	clean := strings.Trim(lower, "[](){}:;,. ")
	if normalizeVersion(clean) == target {
		return 1
	}
	if match := rangeToken.FindStringSubmatch(lower); match != nil && versionInRange(target, normalizeVersion(match[1]), normalizeVersion(match[2])) {
		return .85
	}
	for _, match := range versionToken.FindAllStringSubmatch(lower, -1) {
		if normalizeVersion(match[1]) == target {
			return .9
		}
	}
	return 0
}

func normalizeVersion(version string) string {
	v := strings.ToLower(strings.TrimSpace(version))
	v = strings.TrimPrefix(v, "refs/tags/")
	v = strings.TrimPrefix(v, "release-")
	v = strings.TrimPrefix(v, "v")
	return v
}

func versionInRange(target, a, b string) bool {
	if compareVersion(a, b) > 0 {
		a, b = b, a
	}
	return compareVersion(target, a) >= 0 && compareVersion(target, b) <= 0
}

func compareVersion(a, b string) int {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for len(aa) < len(bb) {
		aa = append(aa, "0")
	}
	for len(bb) < len(aa) {
		bb = append(bb, "0")
	}
	for i := range aa {
		ai, aerr := strconv.Atoi(aa[i])
		bi, berr := strconv.Atoi(bb[i])
		if aerr == nil && berr == nil {
			if ai < bi {
				return -1
			}
			if ai > bi {
				return 1
			}
			continue
		}
		if aa[i] < bb[i] {
			return -1
		}
		if aa[i] > bb[i] {
			return 1
		}
	}
	return 0
}
