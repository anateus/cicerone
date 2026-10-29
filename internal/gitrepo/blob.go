package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/anateus/cicerone/internal/execx"
)

// BlobReader reads one history blob at a time from a single git cat-file
// process. It is not safe for concurrent use. Close must be called when done.
type BlobReader struct {
	ctx      context.Context
	runner   execx.DuplexRunner
	resolver execx.Runner
	path     string
	stream   io.ReadWriteCloser
	reader   *bufio.Reader
	closeErr error
	failed   error
	closed   bool
}

// OpenBlobReader starts a persistent cat-file process.
func (r Repository) OpenBlobReader(ctx context.Context) (*BlobReader, error) {
	runner, ok := r.runner.(execx.DuplexRunner)
	if !ok {
		return nil, fmt.Errorf("read blobs: runner does not support bidirectional processes")
	}
	reader := &BlobReader{ctx: ctx, runner: runner, resolver: r.runner, path: r.source.Path}
	if err := reader.start(); err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *BlobReader) start() error {
	stream, err := r.runner.Start(r.ctx, "git", "-C", r.path, "cat-file", "--batch=ok %(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return fmt.Errorf("start blob reader: %w", err)
	}
	r.stream = stream
	r.reader = bufio.NewReader(stream)
	return nil
}

// Blob reads exactly the bytes of revision:path without a shell. Paths with
// line separators are resolved to object IDs before entering the batch
// protocol. Only one response is in flight and no previous blobs are retained.
func (r *BlobReader) Blob(revision, path string) ([]byte, error) {
	if r.closed {
		return nil, errors.New("read blob: reader is closed")
	}
	if err := r.ctx.Err(); err != nil {
		return nil, fmt.Errorf("read blob: %w", err)
	}
	if r.failed != nil {
		return nil, fmt.Errorf("read blob: reader is unusable: %w", r.failed)
	}
	// History uses revisions without whitespace or a colon. This restriction
	// also distinguishes the success marker from a missing response, which
	// echoes the input expression verbatim.
	if revision == "" || strings.HasPrefix(revision, "-") || strings.ContainsAny(revision, ":\x00 \t\r\n") || path == "" || strings.ContainsRune(path, '\x00') {
		return nil, errors.New("read blob: invalid revision or path")
	}
	expression := revision + ":" + path
	resolve := strings.ContainsAny(path, "\r\n")
	body, firstErr := r.readBlob(expression, resolve)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return nil, errors.Join(execx.CancellationError(r.ctx, firstErr), ctxErr)
	}
	if firstErr == nil {
		return body, nil
	}

	// A partial clone may fail to fetch an object transiently. Match Blob's
	// existing one-retry behavior, but restart after any failed response so
	// a broken process or protocol stream cannot contaminate the retry.
	closeErr := r.stream.Close()
	r.stream = nil
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		r.failed = errors.Join(firstErr, execx.CancellationError(r.ctx, closeErr), ctxErr)
		return nil, r.failed
	}
	if cleanupErr := nonProcessErrors(closeErr); cleanupErr != nil {
		r.failed = errors.Join(firstErr, closeErr)
		return nil, r.failed
	}
	if err := r.start(); err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		}
		r.failed = errors.Join(firstErr, closeErr, err)
		return nil, r.failed
	}
	r.failed = nil
	body, err := r.readBlob(expression, resolve)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return nil, errors.Join(firstErr, closeErr, execx.CancellationError(r.ctx, err), ctxErr)
	}
	if err != nil {
		return nil, errors.Join(firstErr, closeErr, err)
	}
	return body, nil
}

// nonProcessErrors retains pipe and forced-termination failures while allowing
// a failed Git exit to be superseded by a successful retry.
func nonProcessErrors(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*execx.ProcessError); ok {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result error
		for _, part := range joined.Unwrap() {
			result = errors.Join(result, nonProcessErrors(part))
		}
		return result
	}
	return err
}

func (r *BlobReader) readBlob(expression string, resolve bool) ([]byte, error) {
	wrap := func(err error) error {
		if ctxErr := r.ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(ctxErr, err)
		}
		return fmt.Errorf("read blob %s: %w", expression, err)
	}
	fail := func(err error) ([]byte, error) {
		r.failed = wrap(execx.CancellationError(r.ctx, err))
		return nil, r.failed
	}
	query := expression
	if resolve {
		// Older Git versions do not support cat-file's NUL input mode. Use a
		// direct argument, not a pathspec or shell, for the uncommon paths that
		// cannot be represented safely as one line.
		result, err := r.resolver.Run(r.ctx, "git", "-C", r.path, "rev-parse", "--verify", expression)
		if err != nil {
			return nil, wrap(fmt.Errorf("resolve object: %w", err))
		}
		query = strings.TrimSpace(string(result.Stdout))
		if !isObjectID(query) {
			return nil, wrap(errors.New("malformed resolved object ID"))
		}
	}
	n, err := io.WriteString(r.stream, query+"\n")
	if err != nil {
		return fail(err)
	}
	if n != len(query)+1 {
		return fail(io.ErrShortWrite)
	}

	prefix, err := r.reader.Peek(3)
	if err != nil {
		return fail(fmt.Errorf("read response header: %w", err))
	}
	if string(prefix) != "ok " {
		// Missing and ambiguous responses echo the query verbatim.
		echo := make([]byte, len(query))
		if _, err := io.ReadFull(r.reader, echo); err != nil {
			return fail(fmt.Errorf("read response expression: %w", err))
		}
		if string(echo) != query {
			return fail(errors.New("malformed cat-file response expression"))
		}
		status, err := r.reader.ReadSlice('\n')
		if err != nil {
			return fail(fmt.Errorf("read response status: %w", err))
		}
		if string(status) != " missing\n" && string(status) != " ambiguous\n" {
			return fail(fmt.Errorf("malformed cat-file response status %q", status))
		}
		return nil, fmt.Errorf("read blob %s: %s", expression, strings.TrimSpace(string(status)))
	}

	header, err := r.reader.ReadSlice('\n')
	if err != nil {
		return fail(fmt.Errorf("read response header: %w", err))
	}
	fields := strings.Fields(string(header))
	if len(fields) != 4 || fields[0] != "ok" || !isObjectID(fields[1]) {
		return fail(fmt.Errorf("malformed cat-file response header %q", header))
	}
	size, err := strconv.ParseUint(fields[3], 10, 63)
	if err != nil || size > uint64(^uint(0)>>1) {
		return fail(fmt.Errorf("invalid cat-file object size %q", fields[3]))
	}
	body, err := io.ReadAll(io.LimitReader(r.reader, int64(size)))
	if err != nil {
		return fail(fmt.Errorf("read object body: %w", err))
	}
	if uint64(len(body)) != size {
		return fail(fmt.Errorf("read object body: %w", io.ErrUnexpectedEOF))
	}
	separator, err := r.reader.ReadByte()
	if err != nil {
		return fail(fmt.Errorf("read object terminator: %w", err))
	}
	if separator != '\n' {
		return fail(errors.New("malformed cat-file object terminator"))
	}
	if fields[2] != "blob" {
		return nil, fmt.Errorf("read blob %s: object has type %s, not blob", expression, fields[2])
	}
	return body, nil
}

// Close closes the process input and output and waits for Git. It is safe to
// call more than once.
func (r *BlobReader) Close() error {
	if !r.closed {
		r.closed = true
		if r.stream != nil {
			r.closeErr = execx.CancellationError(r.ctx, r.stream.Close())
		}
	}
	return r.closeErr
}
