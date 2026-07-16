package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"

	_ "modernc.org/sqlite"
)

type writeRequest struct {
	ctx  context.Context
	fn   func(*sql.Tx) error
	done chan error
}

// Store owns the database and serializes all write transactions.
type Store struct {
	db      *sql.DB
	writes  chan writeRequest
	stop    chan struct{}
	stopped chan struct{}
	closeMu sync.Mutex
	closed  bool
}

// Open opens a SQLite store, applies embedded migrations, and starts its writer.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = (&url.URL{Scheme: "file", Path: path}).String()
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, writes: make(chan writeRequest), stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.runWriter()
	return s, nil
}

func (s *Store) runWriter() {
	defer close(s.stopped)
	for {
		select {
		case request := <-s.writes:
			request.done <- s.write(request.ctx, request.fn)
		case <-s.stop:
			return
		}
	}
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Write runs fn in a transaction on the store's single writer goroutine.
func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	request := writeRequest{ctx: ctx, fn: fn, done: make(chan error, 1)}
	select {
	case s.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("store is closed")
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the writer and closes the database.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.stop)
	<-s.stopped
	return s.db.Close()
}
