package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const maxSyncErrorBytes = 1024

type SyncResult struct {
	Cursor              string
	Events, Diagnostics int
}
type SyncRunStatus struct {
	Source                          string
	Started, Completed, LastSuccess time.Time
	Cursor                          string
	Events, Diagnostics             int
	Error                           string
}

// FreshnessStatus compares the newest successful repository synchronization
// with the newest package update currently indexed.
type FreshnessStatus struct {
	LastSync          time.Time
	LastPackageUpdate time.Time
}

func (s *Store) LatestFreshness(ctx context.Context) (FreshnessStatus, error) {
	var lastSync, lastUpdate int64
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(last_success_at) FROM sync_runs), 0),
		COALESCE((SELECT MAX(event_time) FROM update_events), 0)`).Scan(&lastSync, &lastUpdate)
	if err != nil {
		return FreshnessStatus{}, err
	}
	var status FreshnessStatus
	if lastSync != 0 {
		status.LastSync = time.Unix(0, lastSync).UTC()
	}
	if lastUpdate != 0 {
		status.LastPackageUpdate = time.Unix(0, lastUpdate).UTC()
	}
	return status, nil
}

func (s *Store) SyncStarted(ctx context.Context, source string, at time.Time) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repositories(id) VALUES(?) ON CONFLICT(id) DO NOTHING`, source); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sync_runs(repository_id,started_at) VALUES(?,?)`, source, at.UnixNano())
		return err
	})
}

func (s *Store) SyncFinished(ctx context.Context, source string, at time.Time, result SyncResult, syncErr error) error {
	errorText := ""
	if syncErr != nil {
		errorText = strings.TrimSpace(syncErr.Error())
		if len(errorText) > maxSyncErrorBytes {
			errorText = errorText[:maxSyncErrorBytes]
		}
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		var previous sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT last_success_at FROM sync_runs WHERE repository_id=? AND last_success_at IS NOT NULL ORDER BY id DESC LIMIT 1`, source).Scan(&previous)
		lastSuccess := previous
		if syncErr == nil {
			lastSuccess = sql.NullInt64{Int64: at.UnixNano(), Valid: true}
		}
		updated, err := tx.ExecContext(ctx, `UPDATE sync_runs SET completed_at=?,error=?,cursor=?,event_count=?,diagnostic_count=?,last_success_at=? WHERE id=(SELECT id FROM sync_runs WHERE repository_id=? AND completed_at IS NULL ORDER BY id DESC LIMIT 1)`, at.UnixNano(), errorText, result.Cursor, result.Events, result.Diagnostics, lastSuccess, source)
		if err != nil {
			return err
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("no active sync run for %s", source)
		}
		return nil
	})
}

func (s *Store) SyncStatus(ctx context.Context, source string) (SyncRunStatus, bool, error) {
	var status SyncRunStatus
	var started int64
	var completed, success sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT repository_id,started_at,completed_at,cursor,event_count,diagnostic_count,error,last_success_at FROM sync_runs WHERE repository_id=? ORDER BY id DESC LIMIT 1`, source).Scan(&status.Source, &started, &completed, &status.Cursor, &status.Events, &status.Diagnostics, &status.Error, &success)
	if err == sql.ErrNoRows {
		return SyncRunStatus{}, false, nil
	}
	if err != nil {
		return SyncRunStatus{}, false, err
	}
	status.Started = time.Unix(0, started).UTC()
	if completed.Valid {
		status.Completed = time.Unix(0, completed.Int64).UTC()
	}
	if success.Valid {
		status.LastSuccess = time.Unix(0, success.Int64).UTC()
	}
	return status, true, nil
}
