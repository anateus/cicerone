package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/anateus/cicerone/internal/execx"
)

// BlobRef identifies the exact definition needed for a history comparison.
type BlobRef struct {
	Revision string
	Path     string
}

// BlobPrefetcher hydrates missing definition blobs in an owned partial clone.
// Repositories with an unsupported Git or a more complex promisor configuration
// continue to use Git's ordinary on-demand object fetching.
type BlobPrefetcher struct {
	repository Repository
	runner     execx.DuplexRunner
	enabled    bool
}

const blobPrefetchLimit = 64

// NewBlobPrefetcher enables targeted bulk fetch only for our own blobless
// mirrors with one known promisor. It does not contact the remote.
func (r Repository) NewBlobPrefetcher(ctx context.Context) (*BlobPrefetcher, error) {
	p := &BlobPrefetcher{repository: r}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !r.source.Owned || r.source.RemoteURL == "" {
		return p, nil
	}
	// Honor an explicit request to disable Git's on-demand fetching.
	if value, ok := os.LookupEnv("GIT_NO_LAZY_FETCH"); ok {
		switch strings.ToLower(value) {
		case "", "0", "false", "no", "off":
		default:
			return p, nil
		}
	}
	runner, ok := r.runner.(execx.DuplexRunner)
	if !ok {
		return p, nil
	}
	// --no-lazy-fetch is not available on every supported Git. Never use an
	// unchecked batch-check on older versions: it would fetch one blob at a time.
	if _, err := r.runner.Run(ctx, "git", "--no-lazy-fetch", "version"); err != nil {
		if ctx.Err() != nil {
			return nil, execx.CancellationError(ctx, err)
		}
		return p, nil
	}
	remote, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "remote", "get-url", "origin")
	if err != nil {
		return nil, fmt.Errorf("read prefetch origin: %w", err)
	}
	if strings.TrimSpace(string(remote.Stdout)) != r.source.RemoteURL {
		return p, nil
	}
	config, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "config", "--null", "--list")
	if err != nil {
		return nil, fmt.Errorf("read prefetch configuration: %w", err)
	}
	originPromisor, originBlobless := false, false
	for _, entry := range strings.Split(string(config.Stdout), "\x00") {
		key, value, found := strings.Cut(entry, "\n")
		if !found {
			continue
		}
		if key == "extensions.partialclone" {
			if value != "origin" {
				return p, nil
			}
			originPromisor = true
			continue
		}
		if !strings.HasPrefix(key, "remote.") {
			continue
		}
		name := strings.TrimPrefix(key, "remote.")
		switch {
		case strings.HasSuffix(name, ".promisor"):
			name = strings.TrimSuffix(name, ".promisor")
			switch strings.ToLower(value) {
			case "true", "yes", "on", "1":
				if name != "origin" {
					return p, nil
				}
				originPromisor = true
			case "false", "no", "off", "0":
			default:
				return p, nil
			}
		case strings.HasSuffix(name, ".partialclonefilter"):
			name = strings.TrimSuffix(name, ".partialclonefilter")
			if name != "origin" {
				return p, nil
			}
			originPromisor = true
			originBlobless = value == "blob:none"
		}
	}
	p.enabled = originPromisor && originBlobless
	p.runner = runner
	return p, nil
}

// Enabled reports whether this repository supports targeted bulk hydration.
func (p *BlobPrefetcher) Enabled() bool {
	return p != nil && p.enabled
}

// Prefetch resolves exact revision/path pairs without reading their blob bodies,
// then fetches only the missing object IDs, at most 64 per Git fetch. Empty
// requests never invoke fetch, whose default refspec would otherwise apply.
func (p *BlobPrefetcher) Prefetch(ctx context.Context, refs []BlobRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || !p.enabled || len(refs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(refs))
	objects := make([]string, 0, len(refs))
	// Bound both the number and total argument bytes, including arbitrary Git
	// path bytes; revision:path is passed directly, never as a pathspec or shell.
	for start := 0; start < len(refs); {
		args := []string{"-C", p.repository.source.Path, "rev-parse", "--revs-only"}
		end, bytes := start, 0
		for end < len(refs) && end-start < blobPrefetchLimit {
			ref := refs[end]
			if ref.Revision == "" || strings.HasPrefix(ref.Revision, "-") || strings.ContainsAny(ref.Revision, ":\x00 \t\r\n") || ref.Path == "" || strings.ContainsRune(ref.Path, '\x00') {
				return errors.New("prefetch blob: invalid revision or path")
			}
			expression := ref.Revision + ":" + ref.Path
			if end > start && bytes+len(expression) > 32*1024 {
				break
			}
			args = append(args, expression)
			bytes += len(expression)
			end++
		}
		result, err := p.repository.runner.Run(ctx, "git", args...)
		if err != nil {
			return fmt.Errorf("resolve prefetch blobs: %w", err)
		}
		ids := strings.Fields(string(result.Stdout))
		if len(ids) != end-start {
			return fmt.Errorf("resolve prefetch blobs: got %d object IDs for %d paths", len(ids), end-start)
		}
		for _, id := range ids {
			if !isObjectID(id) {
				return fmt.Errorf("resolve prefetch blobs: malformed object ID %q", id)
			}
			if !seen[id] {
				seen[id] = true
				objects = append(objects, id)
			}
		}
		start = end
	}
	missing, err := p.missing(ctx, objects)
	if err != nil {
		return err
	}
	for start := 0; start < len(missing); start += blobPrefetchLimit {
		end := min(start+blobPrefetchLimit, len(missing))
		args := []string{"-C", p.repository.source.Path, "-c", "fetch.negotiationAlgorithm=noop", "fetch", "origin", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none"}
		args = append(args, missing[start:end]...)
		if _, err := p.repository.runner.Run(ctx, "git", args...); err != nil {
			return fmt.Errorf("fetch missing history blobs: %w", err)
		}
	}
	if len(missing) > 0 {
		stillMissing, err := p.missing(ctx, missing)
		if err != nil {
			return err
		}
		if len(stillMissing) > 0 {
			return fmt.Errorf("fetch missing history blobs: %d requested objects are still missing (first %s)", len(stillMissing), stillMissing[0])
		}
	}
	return nil
}

func (p *BlobPrefetcher) missing(ctx context.Context, objects []string) (missing []string, err error) {
	if len(objects) == 0 {
		return nil, nil
	}
	stream, err := p.runner.Start(ctx, "git", "--no-lazy-fetch", "-C", p.repository.source.Path, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, fmt.Errorf("start local blob check: %w", err)
	}
	defer func() { err = errors.Join(err, execx.CancellationError(ctx, stream.Close())) }()
	reader := bufio.NewReader(stream)
	for _, id := range objects {
		n, err := io.WriteString(stream, id+"\n")
		if err != nil {
			return nil, fmt.Errorf("check local blob %s: %w", id, execx.CancellationError(ctx, err))
		}
		if n != len(id)+1 {
			return nil, fmt.Errorf("check local blob %s: %w", id, io.ErrShortWrite)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("check local blob %s: %w", id, execx.CancellationError(ctx, err))
		}
		switch line {
		case id + " missing\n":
			missing = append(missing, id)
		case id + " blob\n":
		default:
			return nil, fmt.Errorf("check local blob %s: unexpected response %q", id, line)
		}
	}
	return missing, nil
}
