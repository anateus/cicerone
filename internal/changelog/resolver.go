package changelog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"cicerone/internal/domain"
	"cicerone/internal/store"
)

type PackageRef struct {
	Name, FullName, Homepage, RepositoryURL string
	Type                                    domain.PackageType
}

type Resolver struct {
	Store      *store.Store
	Client     *http.Client
	APIBaseURL string
	Now        func() time.Time
}

func NewResolver(cache *store.Store, client *http.Client) *Resolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Resolver{Store: cache, Client: client, APIBaseURL: "https://api.github.com", Now: func() time.Time { return time.Now().UTC() }}
}

func (r *Resolver) Resolve(ctx context.Context, pkg PackageRef, version string) (Section, error) {
	if r.Store == nil {
		return Section{}, errors.New("changelog resolver store is nil")
	}
	id := pkg.FullName
	if id == "" {
		id = pkg.Name
	}
	if id == "" {
		return Section{}, errors.New("package name is empty")
	}
	if err := r.Store.UpsertChangelogPackage(ctx, id, pkg.Name, string(pkg.Type)); err != nil {
		return Section{}, err
	}
	cached, err := r.Store.ChangelogArtifacts(ctx, id)
	if err != nil {
		return Section{}, err
	}
	if section, ok := MatchVersion(version, fromStoreArtifacts(cached)); ok {
		return section, nil
	}
	var cachedSection Section
	for _, artifact := range cached {
		sections, sectionErr := r.Store.ChangelogSections(ctx, artifact.ID)
		if sectionErr != nil {
			return Section{}, sectionErr
		}
		for _, candidate := range sections {
			if normalizeVersion(candidate.Version) == normalizeVersion(version) && candidate.Confidence > cachedSection.Confidence {
				cachedSection = Section{ArtifactID: candidate.ArtifactID, Version: candidate.Version, Body: candidate.Body, Confidence: candidate.Confidence, SourceURL: candidate.SourceURL}
			}
		}
	}
	if cachedSection.Confidence > 0 {
		return cachedSection, nil
	}
	owner, repo, ok := githubRepository(pkg.RepositoryURL)
	if !ok {
		return Section{}, fmt.Errorf("unsupported repository URL %q", pkg.RepositoryURL)
	}
	backoffKey := strings.TrimRight(r.APIBaseURL, "/") + "/repos/" + owner + "/" + repo
	retryAfter, err := r.Store.ChangelogRetryAfter(ctx, backoffKey)
	if err != nil {
		return Section{}, err
	}
	if r.Now().Before(retryAfter) {
		return Section{}, fmt.Errorf("changelog refresh backed off until %s", retryAfter.Format(time.RFC3339))
	}
	section, err := r.resolveRepositoryFiles(ctx, id, owner, repo, version)
	if err == nil {
		return section, nil
	}
	section, releaseErr := r.resolveRelease(ctx, id, pkg.Name, owner, repo, version)
	if releaseErr == nil {
		return section, nil
	}
	combined := fmt.Errorf("repository changelog: %v; GitHub release: %w", err, releaseErr)
	_ = r.Store.RecordChangelogFailure(ctx, backoffKey, r.Now(), combined)
	return Section{}, combined
}

type githubFile struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
}

func (r *Resolver) resolveRepositoryFiles(ctx context.Context, packageID, owner, repo, version string) (Section, error) {
	endpoint := strings.TrimRight(r.APIBaseURL, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents"
	var files []githubFile
	if err := r.getJSON(ctx, endpoint, &files); err != nil {
		return Section{}, err
	}
	sort.SliceStable(files, func(i, j int) bool {
		ri, rj := changelogFileRank(files[i].Name), changelogFileRank(files[j].Name)
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	foundCandidate := false
	for _, file := range files {
		if changelogFileRank(file.Name) >= 100 || file.DownloadURL == "" {
			continue
		}
		foundCandidate = true
		body, media, etag, lastModified, err := r.getBytes(ctx, file.DownloadURL)
		if err != nil {
			continue
		}
		artifact, err := r.persistArtifact(ctx, packageID, file.DownloadURL, media, etag, lastModified, body)
		if err != nil {
			return Section{}, err
		}
		if section, ok := MatchVersion(version, []Artifact{artifact}); ok && section.Confidence >= .8 {
			if err := r.persistSection(ctx, section); err != nil {
				return Section{}, err
			}
			return section, nil
		}
	}
	if !foundCandidate {
		return Section{}, errors.New("no repository changelog candidate")
	}
	return Section{}, fmt.Errorf("repository changelog has no section for %s", version)
}

func (r *Resolver) resolveRelease(ctx context.Context, packageID, packageName, owner, repo, version string) (Section, error) {
	base := strings.TrimRight(r.APIBaseURL, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases/tags/"
	var last error
	for _, tag := range normalizedTagCandidates(packageName, version) {
		var release struct {
			TagName string `json:"tag_name"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		}
		if err := r.getJSON(ctx, base+url.PathEscape(tag), &release); err != nil {
			last = err
			continue
		}
		if release.Body == "" {
			last = errors.New("release body is empty")
			continue
		}
		source := release.HTMLURL
		if source == "" {
			source = base + url.PathEscape(tag)
		}
		artifact, err := r.persistArtifact(ctx, packageID, source, "text/markdown", "", "", []byte(release.Body))
		if err != nil {
			return Section{}, err
		}
		section := Section{ArtifactID: artifact.ID, Version: version, Body: release.Body, Confidence: 1, SourceURL: source}
		if err := r.persistSection(ctx, section); err != nil {
			return Section{}, err
		}
		return section, nil
	}
	if last == nil {
		last = errors.New("no release tag candidates")
	}
	return Section{}, last
}

func (r *Resolver) persistArtifact(ctx context.Context, packageID, source, media, etag, lastModified string, body []byte) (Artifact, error) {
	sum := sha256.Sum256(body)
	a := store.ChangelogArtifact{URL: source, MediaType: media, ETag: etag, LastModified: lastModified, Hash: hex.EncodeToString(sum[:]), Raw: append([]byte(nil), body...), Extracted: append([]byte(nil), body...), FetchedAt: r.Now()}
	saved, err := r.Store.SaveChangelogArtifact(ctx, packageID, a)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{ID: saved.ID, URL: saved.URL, MediaType: saved.MediaType, ETag: saved.ETag, LastModified: saved.LastModified, Hash: saved.Hash, Raw: saved.Raw, Extracted: saved.Extracted, FetchedAt: saved.FetchedAt, ParentID: saved.ParentID}, nil
}

func (r *Resolver) persistSection(ctx context.Context, s Section) error {
	return r.Store.SaveChangelogSection(ctx, store.ChangelogSection{ArtifactID: s.ArtifactID, Version: s.Version, Body: s.Body, Confidence: s.Confidence, SourceURL: s.SourceURL})
}

func (r *Resolver) request(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	return resp, nil
}

func (r *Resolver) getJSON(ctx context.Context, endpoint string, dst any) error {
	resp, err := r.request(ctx, endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst)
}
func (r *Resolver) getBytes(ctx context.Context, endpoint string) ([]byte, string, string, string, error) {
	resp, err := r.request(ctx, endpoint)
	if err != nil {
		return nil, "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), err
}

func githubRepository(raw string) (string, string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}
func normalizedTagCandidates(name, version string) []string {
	v := normalizeVersion(version)
	values := []string{version, v, "v" + v}
	if name != "" {
		values = append(values, name+"-"+v, name+"-v"+v)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
func changelogFileRank(name string) int {
	n := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(n, "CHANGELOG"):
		return 0
	case strings.HasPrefix(n, "CHANGES"):
		return 1
	case strings.HasPrefix(n, "NEWS"):
		return 2
	case n == "HISTORY":
		return 3
	case n == "HISTORY.MD":
		return 4
	case n == "HISTORY.TXT":
		return 5
	case strings.HasPrefix(n, "RELEASES"):
		return 6
	case strings.Contains(n, "WHATSNEW"):
		return 7
	default:
		return 100
	}
}
func fromStoreArtifacts(values []store.ChangelogArtifact) []Artifact {
	out := make([]Artifact, len(values))
	for i, a := range values {
		out[i] = Artifact{ID: a.ID, URL: a.URL, MediaType: a.MediaType, ETag: a.ETag, LastModified: a.LastModified, Hash: a.Hash, Raw: a.Raw, Extracted: a.Extracted, FetchedAt: a.FetchedAt, ParentID: a.ParentID}
	}
	return out
}
