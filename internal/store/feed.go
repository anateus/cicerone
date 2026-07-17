package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"cicerone/internal/domain"
)

// UpsertEvents atomically stores immutable update events and their packages.
func (s *Store) UpsertEvents(ctx context.Context, events []domain.UpdateEvent) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, event := range events {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO packages(id, name, type) VALUES (?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type`,
				event.PackageID, event.Name, event.Type); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO update_events(
					id, package_id, kind, old_version, new_version, old_revision, new_revision,
					repository, definition_path, commit_hash, event_time, diagnostic
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT DO NOTHING`, event.ID, event.PackageID, event.Kind,
				event.OldVersion, event.NewVersion, event.OldRevision, event.NewRevision,
				event.Repository, event.DefinitionPath, event.Commit, event.Time.UnixNano(), event.Diagnostic); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetInstalled atomically replaces the installed-package snapshot.
func (s *Store) SetInstalled(ctx context.Context, packages []domain.InstalledPackage) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM installed_packages`); err != nil {
			return err
		}
		for _, pkg := range packages {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO packages(id, name, type) VALUES (?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type`, pkg.PackageID, pkg.Name, pkg.Type); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO installed_packages(package_id, name, version, type, pinned, upgrade_available)
				VALUES (?, ?, ?, ?, ?, ?)`, pkg.PackageID, pkg.Name, pkg.Version, pkg.Type, pkg.Pinned, pkg.UpgradeAvailable); err != nil {
				return err
			}
		}
		return nil
	})
}

// QueryFeed selects relevant event rows in SQL and applies domain grouping rules.
func (s *Store) QueryFeed(ctx context.Context, filter domain.FeedFilter) ([]domain.FeedGroup, error) {
	where := []string{"1=1"}
	args := make([]any, 0, 8)
	if filter.Horizon > 0 {
		where = append(where, `(e.event_time >= ? OR i.package_id IS NOT NULL)`)
		args = append(args, filter.Now.Add(-filter.Horizon).UnixNano())
	}
	if len(filter.Kinds) > 0 {
		values := trueMapValues(filter.Kinds)
		if len(values) == 0 {
			return nil, nil
		}
		where = append(where, `e.kind IN (`+placeholders(len(values))+`)`)
		for _, value := range values {
			args = append(args, value)
		}
	}
	if len(filter.Types) > 0 {
		values := trueMapValues(filter.Types)
		if len(values) == 0 {
			return nil, nil
		}
		where = append(where, `p.type IN (`+placeholders(len(values))+`)`)
		for _, value := range values {
			args = append(args, value)
		}
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		where = append(where, `(lower(p.name) LIKE ? OR lower(p.id) LIKE ?)`)
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	query := `SELECT e.id, e.package_id, p.name, p.type, e.kind,
		e.old_version, e.new_version, e.old_revision, e.new_revision,
		e.repository, e.definition_path, e.commit_hash, e.event_time, e.diagnostic,
		i.package_id IS NOT NULL
		FROM update_events e JOIN packages p ON p.id=e.package_id
		LEFT JOIN installed_packages i ON i.package_id=e.package_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY e.event_time DESC, e.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]domain.UpdateEvent, 0)
	installed := make(map[domain.PackageID]bool)
	for rows.Next() {
		var event domain.UpdateEvent
		var timestamp int64
		var isInstalled bool
		if err := rows.Scan(&event.ID, &event.PackageID, &event.Name, &event.Type, &event.Kind,
			&event.OldVersion, &event.NewVersion, &event.OldRevision, &event.NewRevision,
			&event.Repository, &event.DefinitionPath, &event.Commit, &timestamp, &event.Diagnostic, &isInstalled); err != nil {
			return nil, err
		}
		event.Time = time.Unix(0, timestamp).UTC()
		event.Installed = isInstalled
		events = append(events, event)
		if isInstalled {
			installed[event.PackageID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return domain.BuildFeed(events, installed, filter), nil
}

// Preferences returns the persisted feed filter preferences.
func (s *Store) Preferences(ctx context.Context) (domain.FeedFilter, error) {
	var horizon int64
	var version, revision, metadata, formula, cask bool
	var filter domain.FeedFilter
	err := s.db.QueryRowContext(ctx, `SELECT horizon_seconds, show_version, show_revision, show_metadata, show_formula, show_cask, query, roll_up FROM preferences WHERE id=1`).
		Scan(&horizon, &version, &revision, &metadata, &formula, &cask, &filter.Query, &filter.RollUp)
	if err != nil {
		return domain.FeedFilter{}, err
	}
	filter.Horizon = time.Duration(horizon) * time.Second
	filter.Kinds = map[domain.EventKind]bool{}
	if version {
		filter.Kinds[domain.EventVersion] = true
	}
	if revision {
		filter.Kinds[domain.EventRevision] = true
	}
	if metadata {
		filter.Kinds[domain.EventMetadata] = true
	}
	filter.Types = map[domain.PackageType]bool{}
	if formula {
		filter.Types[domain.PackageFormula] = true
	}
	if cask {
		filter.Types[domain.PackageCask] = true
	}
	return filter, nil
}

// SetPreferences persists feed filter preferences in typed columns.
func (s *Store) SetPreferences(ctx context.Context, filter domain.FeedFilter) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE preferences SET horizon_seconds=?, show_version=?, show_revision=?, show_metadata=?, show_formula=?, show_cask=?, query=?, roll_up=? WHERE id=1`,
			int64(filter.Horizon/time.Second), filter.Kinds[domain.EventVersion], filter.Kinds[domain.EventRevision], filter.Kinds[domain.EventMetadata],
			filter.Types[domain.PackageFormula], filter.Types[domain.PackageCask], filter.Query, filter.RollUp)
		return err
	})
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func trueMapValues[K ~string](values map[K]bool) []string {
	result := make([]string, 0, len(values))
	for value, enabled := range values {
		if enabled {
			result = append(result, string(value))
		}
	}
	sort.Strings(result)
	return result
}
