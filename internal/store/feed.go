package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/anateus/cicerone/internal/domain"
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

// UpsertCatalogPackages makes Homebrew search hits available to the existing
// package-info cache without inventing history events or replacing user status.
func (s *Store) UpsertCatalogPackages(ctx context.Context, packages []domain.CatalogPackage) error {
	if len(packages) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, pkg := range packages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO packages(id, name, type) VALUES (?, ?, ?)
				ON CONFLICT(id) DO NOTHING`, pkg.ID, pkg.ID, pkg.Type); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetPackageStatus persists the user-assigned status for a package. Hidden is
// stored under the legacy 'snoozed' value that migration 014's CHECK allows.
func (s *Store) SetPackageStatus(ctx context.Context, packageID domain.PackageID, status domain.PackageStatus) error {
	if status == "" {
		status = domain.PackageStatusDefault
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE packages SET status=? WHERE id=?`, packageStatusStorage(status), packageID)
		return err
	})
}

// packageStatusStorage maps domain statuses onto the values allowed by the
// migration-014 CHECK constraint. The groups feature keeps 'snoozed' on disk
// as the storage spelling of hidden.
func packageStatusStorage(status domain.PackageStatus) string {
	if status == domain.PackageStatusHidden {
		return "snoozed"
	}
	return string(status)
}

// packageStatusDomain maps stored status values back onto domain statuses.
func packageStatusDomain(status domain.PackageStatus) domain.PackageStatus {
	if status == "snoozed" {
		return domain.PackageStatusHidden
	}
	return status
}

// groupScopeSQL returns the SQL predicate and arguments that restrict the feed
// to the filter's group scope. Hidden packages are excluded from every scope
// except the hidden one itself.
func groupScopeSQL(filter domain.FeedFilter) (string, []any) {
	switch filter.GroupScope {
	case domain.GroupScopeUngrouped:
		return `p.group_id IS NULL AND p.status != 'snoozed'`, nil
	case domain.GroupScopeStarred:
		return `p.status = 'starred'`, nil
	case domain.GroupScopeHidden:
		return `p.status = 'snoozed'`, nil
	case domain.GroupScopeUser:
		return `p.group_id = ? AND p.status != 'snoozed'`, []any{int64(filter.GroupTarget)}
	default:
		return `p.status != 'snoozed'`, nil
	}
}

// QueryFeed selects relevant event rows in SQL and applies domain grouping rules.
func (s *Store) QueryFeed(ctx context.Context, filter domain.FeedFilter) ([]domain.FeedGroup, error) {
	where := []string{"1=1"}
	args := make([]any, 0, 8)
	scopePredicate, scopeArgs := groupScopeSQL(filter)
	where = append(where, scopePredicate)
	args = append(args, scopeArgs...)
	if filter.Horizon > 0 {
		horizon := `(e.event_time >= ? OR i.package_id IS NOT NULL`
		args = append(args, filter.Now.Add(-filter.Horizon).UnixNano())
		if strings.TrimSpace(filter.Query) != "" && len(filter.ExternalMatches) > 0 {
			horizon += ` OR p.id IN (` + placeholders(len(filter.ExternalMatches)) + `)`
			for _, id := range filter.ExternalMatches {
				args = append(args, id)
			}
		}
		where = append(where, horizon+`)`)
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
	if match, ok := feedFTSQuery(filter.Query); ok {
		scope := filter.Search
		if scope == "" {
			scope = domain.SearchNames
		}
		matches := []string{`p.rowid IN (
			SELECT rowid FROM packages_fts WHERE packages_fts MATCH ?
		)`}
		args = append(args, match)
		if searchIncludes(scope, domain.SearchDescriptions) {
			matches = append(matches, `p.id IN (
				SELECT pi.package_id FROM package_info pi
				JOIN package_info_fts ON package_info_fts.rowid=pi.rowid
				WHERE package_info_fts MATCH ?
			)`)
			args = append(args, match)
		}
		if searchIncludes(scope, domain.SearchChangelogs) {
			matches = append(matches, `p.id IN (
				SELECT l.package_id FROM package_document_links l
				JOIN package_documents_fts ON package_documents_fts.rowid=l.document_id
				WHERE l.kind='changelog' AND package_documents_fts MATCH ?
			)`)
			args = append(args, match)
		}
		if searchIncludes(scope, domain.SearchREADMEs) {
			matches = append(matches, `p.id IN (
				SELECT l.package_id FROM package_document_links l
				JOIN package_documents_fts ON package_documents_fts.rowid=l.document_id
				WHERE l.kind='readme' AND package_documents_fts MATCH ?
			)`)
			args = append(args, match)
		}
		if len(filter.ExternalMatches) > 0 {
			matches = append(matches, `p.id IN (`+placeholders(len(filter.ExternalMatches))+`)`)
			for _, packageID := range filter.ExternalMatches {
				args = append(args, packageID)
			}
		}
		where = append(where, `(`+strings.Join(matches, ` OR `)+`)`)
	}
	query := `SELECT e.id, e.package_id, p.name, p.type, p.status, COALESCE(p.group_id, 0), e.kind,
		e.old_version, e.new_version, e.old_revision, e.new_revision,
		e.repository, e.definition_path, e.commit_hash, e.event_time, e.diagnostic, e.seen,
		i.package_id IS NOT NULL,
		COALESCE(c.version_count, 0), COALESCE(c.first_update, 0), COALESCE(c.last_update, 0)
		FROM update_events e JOIN packages p ON p.id=e.package_id
		LEFT JOIN installed_packages i ON i.package_id=e.package_id
		LEFT JOIN (
			SELECT package_id, COUNT(*) AS version_count,
				MIN(event_time) AS first_update, MAX(event_time) AS last_update
			FROM update_events
			WHERE kind='version'
			GROUP BY package_id
		) c ON c.package_id=e.package_id
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
		var versionCount int
		var firstUpdate, lastUpdate int64
		var isInstalled bool
		if err := rows.Scan(&event.ID, &event.PackageID, &event.Name, &event.Type, &event.Status, &event.GroupID, &event.Kind,
			&event.OldVersion, &event.NewVersion, &event.OldRevision, &event.NewRevision,
			&event.Repository, &event.DefinitionPath, &event.Commit, &timestamp, &event.Diagnostic, &event.Seen, &isInstalled,
			&versionCount, &firstUpdate, &lastUpdate); err != nil {
			return nil, err
		}
		event.Time = time.Unix(0, timestamp).UTC()
		event.Status = packageStatusDomain(event.Status)
		event.Installed = isInstalled
		if versionCount > 0 {
			first, last := time.Unix(0, firstUpdate).UTC(), time.Unix(0, lastUpdate).UTC()
			event.Cadence = domain.ClassifyUpdateCadence(versionCount, first, last)
			event.UpdateInterval = domain.AverageUpdateInterval(versionCount, first, last)
		}
		events = append(events, event)
		if isInstalled {
			installed[event.PackageID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter.Query) != "" && len(filter.ExternalMatches) > 0 {
		filter.HorizonExempt = make(map[domain.PackageID]bool, len(filter.ExternalMatches))
		for _, id := range filter.ExternalMatches {
			filter.HorizonExempt[id] = true
		}
	}
	filter.Query = ""
	groups := domain.BuildFeed(events, installed, filter)
	if len(filter.CatalogPackages) == 0 {
		return groups, nil
	}
	// Catalog rows have no revision or metadata event to display. Keep them out
	// when the version-update feed has been explicitly switched off.
	if len(filter.Kinds) > 0 && !filter.Kinds[domain.EventVersion] {
		return groups, nil
	}
	// Show catalog hits without history after real updates. Query the packages
	// table for persisted status and installed state, rather than fabricating an
	// update timestamp, version or an installed flag.
	shown := make(map[domain.PackageID]bool)
	for _, group := range groups {
		for _, event := range group.Events {
			shown[event.PackageID] = true
		}
	}
	for _, pkg := range filter.CatalogPackages {
		if shown[pkg.ID] || len(filter.Types) > 0 && !filter.Types[pkg.Type] {
			continue
		}
		var name string
		var kind domain.PackageType
		var status domain.PackageStatus
		var groupID domain.PackageGroupID
		var isInstalled bool
		var version, description string
		err := s.db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(json_extract(pi.normalized_json, '$.Name'), ''), p.name),
			p.type, p.status, COALESCE(p.group_id, 0), i.package_id IS NOT NULL,
			COALESCE(json_extract(pi.normalized_json, '$.StableVersion'), ''), COALESCE(pi.description, '')
			FROM packages p LEFT JOIN installed_packages i ON i.package_id=p.id
			LEFT JOIN package_info pi ON pi.package_id=p.id WHERE p.id=?`, pkg.ID).
			Scan(&name, &kind, &status, &groupID, &isInstalled, &version, &description)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(filter.Types) > 0 && !filter.Types[kind] {
			continue
		}
		if !domain.MatchesGroupScope(filter.GroupScope, groupID, packageStatusDomain(status), filter.GroupTarget) {
			continue
		}
		status = packageStatusDomain(status)
		shown[pkg.ID] = true
		if description == "" {
			description = pkg.Description
		}
		id := domain.EventID("catalog:" + string(pkg.ID))
		groups = append(groups, domain.FeedGroup{ID: id, Events: []domain.UpdateEvent{{
			ID: id, PackageID: pkg.ID, Name: name, Type: kind, Status: status, GroupID: groupID,
			Kind: domain.EventCatalog, CatalogDescription: description, CatalogVersion: version, Installed: isInstalled, Seen: true,
		}}})
	}
	return groups, nil
}

// MarkEventsSeen records that event rows have appeared in a feed session.
func (s *Store) MarkEventsSeen(ctx context.Context, ids []domain.EventID) error {
	if len(ids) == 0 {
		return nil
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, `UPDATE update_events SET seen=1 WHERE id=?`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, id := range ids {
			if _, err := statement.ExecContext(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Preferences returns the persisted feed filter preferences.
func (s *Store) Preferences(ctx context.Context) (domain.FeedFilter, error) {
	var horizon int64
	var version, revision, metadata, formula, cask bool
	var scopeValue, targetValue int64
	var filter domain.FeedFilter
	err := s.db.QueryRowContext(ctx, `SELECT horizon_seconds, show_version, show_revision, show_metadata,
			show_formula, show_cask, query, search_scope, roll_up, group_scope, group_target FROM preferences WHERE id=1`).
		Scan(&horizon, &version, &revision, &metadata, &formula, &cask, &filter.Query, &filter.Search, &filter.RollUp,
			&scopeValue, &targetValue)
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
	if scopeValue < 0 || scopeValue > int64(domain.GroupScopeUser) {
		scopeValue = 0
	}
	filter.GroupScope = domain.GroupScope(scopeValue)
	filter.GroupTarget = domain.PackageGroupID(targetValue)
	return filter, nil
}

// SetPreferences persists feed filter preferences in typed columns.
func (s *Store) SetPreferences(ctx context.Context, filter domain.FeedFilter) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		scope := filter.Search
		if scope == "" {
			scope = domain.SearchNames
		}
		_, err := tx.ExecContext(ctx, `UPDATE preferences SET horizon_seconds=?, show_version=?, show_revision=?,
				show_metadata=?, show_formula=?, show_cask=?, query=?, search_scope=?, roll_up=?, group_scope=?, group_target=? WHERE id=1`,
			int64(filter.Horizon/time.Second), filter.Kinds[domain.EventVersion], filter.Kinds[domain.EventRevision], filter.Kinds[domain.EventMetadata],
			filter.Types[domain.PackageFormula], filter.Types[domain.PackageCask], filter.Query, scope, filter.RollUp,
			int64(filter.GroupScope), int64(filter.GroupTarget))
		return err
	})
}

func feedFTSQuery(input string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}
	if len(input) >= 2 && input[0] == '"' && input[len(input)-1] == '"' {
		phrase := strings.TrimSpace(input[1 : len(input)-1])
		if phrase == "" {
			return "", false
		}
		return quoteFTS(phrase), true
	}
	tokens := strings.FieldsFunc(input, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	terms := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token != "" {
			terms = append(terms, quoteFTS(token)+"*")
		}
	}
	return strings.Join(terms, " AND "), len(terms) > 0
}

func quoteFTS(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func searchIncludes(scope, category domain.SearchScope) bool {
	order := map[domain.SearchScope]int{
		domain.SearchNames: 0, domain.SearchDescriptions: 1,
		domain.SearchChangelogs: 2, domain.SearchREADMEs: 3,
	}
	return order[scope] >= order[category]
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
