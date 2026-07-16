package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"cicerone/internal/domain"
)

type HistoryState struct {
	Head  string
	Since time.Time
}
type HistoryAlias struct {
	Alias     string
	PackageID domain.PackageID
}
type HistoryBatch struct {
	Repository, Path, Head string
	Since                  time.Time
	Events                 []domain.UpdateEvent
	Aliases                []HistoryAlias
	RemoveCommits          []string
}

func (s *Store) HasHistoryEvent(ctx context.Context, packageID domain.PackageID, kind domain.EventKind) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM update_events WHERE package_id=? AND kind=?)`, packageID, kind).Scan(&found)
	return found, err
}

func (s *Store) HistoryState(ctx context.Context, repository string) (HistoryState, bool, error) {
	var result HistoryState
	var encodedSince string
	err := s.db.QueryRowContext(ctx, `SELECT r.head_commit,COALESCE(g.start_commit,'') FROM repositories r LEFT JOIN repository_ranges g ON g.repository_id=r.id WHERE r.id=?`, repository).Scan(&result.Head, &encodedSince)
	if err == sql.ErrNoRows {
		return HistoryState{}, false, nil
	}
	if err != nil {
		return HistoryState{}, false, err
	}
	if encodedSince != "" {
		result.Since, err = time.Parse(time.RFC3339Nano, encodedSince)
		if err != nil {
			return HistoryState{}, false, err
		}
	}
	return result, true, nil
}

// ApplyHistory makes events, aliases, reconciliation, range and cursor visible atomically.
func (s *Store) ApplyHistory(ctx context.Context, batch HistoryBatch) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if len(batch.RemoveCommits) > 0 {
			args := []any{batch.Repository}
			for _, h := range batch.RemoveCommits {
				args = append(args, h)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM update_events WHERE repository=? AND commit_hash IN (`+placeholders(len(batch.RemoveCommits))+`)`, args...); err != nil {
				return err
			}
		}
		for _, event := range batch.Events {
			if _, err := tx.ExecContext(ctx, `INSERT INTO packages(id,name,type) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,type=excluded.type`, event.PackageID, event.Name, event.Type); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO update_events(id,package_id,kind,old_version,new_version,old_revision,new_revision,repository,definition_path,commit_hash,event_time,diagnostic) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, event.ID, event.PackageID, event.Kind, event.OldVersion, event.NewVersion, event.OldRevision, event.NewRevision, event.Repository, event.DefinitionPath, event.Commit, event.Time.UnixNano(), event.Diagnostic); err != nil {
				return err
			}
		}
		for _, alias := range batch.Aliases {
			if strings.TrimSpace(alias.Alias) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO package_aliases(alias,package_id) VALUES(?,?) ON CONFLICT(alias) DO UPDATE SET package_id=excluded.package_id`, alias.Alias, alias.PackageID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO repositories(id,path,head_commit) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET path=excluded.path,head_commit=excluded.head_commit`, batch.Repository, batch.Path, batch.Head); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM repository_ranges WHERE repository_id=?`, batch.Repository); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO repository_ranges(repository_id,start_commit,end_commit) VALUES(?,?,?)`, batch.Repository, batch.Since.Format(time.RFC3339Nano), batch.Head)
		return err
	})
}
