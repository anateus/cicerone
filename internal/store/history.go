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
	Alias              string
	PackageID          domain.PackageID
	Repository, Commit string
}
type HistoryDiagnostic struct{ Repository, Commit, Path, Message string }
type HistoryBatch struct {
	Repository, Path, Head string
	Since                  time.Time
	Events                 []domain.UpdateEvent
	Aliases                []HistoryAlias
	Diagnostics            []HistoryDiagnostic
	RemoveCommits          []string
}

func (s *Store) HasHistoryEvent(ctx context.Context, repository string, packageID domain.PackageID, kind domain.EventKind) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM update_events WHERE repository=? AND package_id=? AND kind=?)`, repository, packageID, kind).Scan(&found)
	return found, err
}

func (s *Store) ResolveHistoryPackageID(ctx context.Context, repository string, id domain.PackageID) (domain.PackageID, error) {
	var result domain.PackageID
	err := s.db.QueryRowContext(ctx, `SELECT package_id FROM history_aliases WHERE repository=? AND alias=?`, repository, id).Scan(&result)
	if err == sql.ErrNoRows {
		return id, nil
	}
	return result, err
}
func (s *Store) HistoryDiagnostics(ctx context.Context, repository string) ([]HistoryDiagnostic, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository,commit_hash,definition_path,message FROM history_diagnostics WHERE repository=? ORDER BY commit_hash,definition_path,message`, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryDiagnostic
	for rows.Next() {
		var d HistoryDiagnostic
		if err := rows.Scan(&d.Repository, &d.Commit, &d.Path, &d.Message); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
	if strings.TrimSpace(result.Head) == "" {
		return HistoryState{}, false, nil
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
			if _, err := tx.ExecContext(ctx, `DELETE FROM history_aliases WHERE repository=? AND commit_hash IN (`+placeholders(len(batch.RemoveCommits))+`)`, args...); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM history_diagnostics WHERE repository=? AND commit_hash IN (`+placeholders(len(batch.RemoveCommits))+`)`, args...); err != nil {
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
			repository := alias.Repository
			if repository == "" {
				repository = batch.Repository
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO history_aliases(repository,alias,package_id,commit_hash) VALUES(?,?,?,?) ON CONFLICT(repository,alias) DO UPDATE SET package_id=excluded.package_id,commit_hash=excluded.commit_hash`, repository, alias.Alias, alias.PackageID, alias.Commit); err != nil {
				return err
			}
		}
		for _, d := range batch.Diagnostics {
			repository := d.Repository
			if repository == "" {
				repository = batch.Repository
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO history_diagnostics(repository,commit_hash,definition_path,message) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, repository, d.Commit, d.Path, d.Message); err != nil {
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
