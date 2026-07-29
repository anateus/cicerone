package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"cicerone/internal/domain"
)

type ChangelogArtifact struct {
	ID, URL, MediaType, ETag, LastModified, Hash string
	Raw, Extracted                               []byte
	FetchedAt                                    time.Time
	ParentID                                     *string
}

type ChangelogSection struct {
	ArtifactID, Version, Body string
	Confidence                float64
	SourceURL                 string
}

type ChangelogPage struct {
	Sections []ChangelogSection
	NextPage int
}

type ChangelogTarget struct {
	PackageID                                 domain.PackageID
	EventID                                   domain.EventID
	Name, Version, Repository, DefinitionPath string
	Type                                      domain.PackageType
	Commit                                    string
}

func (s *Store) ChangelogTarget(ctx context.Context, packageID domain.PackageID, eventID domain.EventID) (ChangelogTarget, error) {
	var target ChangelogTarget
	err := s.db.QueryRowContext(ctx, `SELECT e.package_id,e.id,p.name,p.type,e.new_version,e.repository,e.definition_path,e.commit_hash
		FROM update_events e JOIN packages p ON p.id=e.package_id WHERE e.id=? AND e.package_id=?`, eventID, packageID).
		Scan(&target.PackageID, &target.EventID, &target.Name, &target.Type, &target.Version, &target.Repository, &target.DefinitionPath, &target.Commit)
	return target, err
}

// LoadChangelog returns cached sections matching the selected event version.
// It performs no network work and is safe on the cached-first startup path.
func (s *Store) LoadChangelog(ctx context.Context, packageID domain.PackageID, eventID domain.EventID) ([]ChangelogSection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cs.artifact_id,cs.version,cs.content,cs.confidence,cs.source_url
		FROM update_events e
		JOIN package_changelog_artifacts pca ON pca.package_id=e.package_id
		JOIN changelog_sections cs ON cs.artifact_id=pca.artifact_id
		WHERE e.id=? AND e.package_id=? AND cs.version=e.new_version
		ORDER BY cs.confidence DESC,cs.id DESC`, eventID, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sections []ChangelogSection
	for rows.Next() {
		var section ChangelogSection
		var artifactID int64
		if err := rows.Scan(&artifactID, &section.Version, &section.Body, &section.Confidence, &section.SourceURL); err != nil {
			return nil, err
		}
		section.ArtifactID = strconv.FormatInt(artifactID, 10)
		sections = append(sections, section)
	}
	return sections, rows.Err()
}

func (s *Store) UpsertChangelogPackage(ctx context.Context, id, name, packageType string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO packages(id,name,type) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,type=excluded.type`, id, name, packageType)
		return err
	})
}

func (s *Store) SaveChangelogArtifact(ctx context.Context, packageID string, artifact ChangelogArtifact) (ChangelogArtifact, error) {
	err := s.Write(ctx, func(tx *sql.Tx) error {
		parent := ""
		if artifact.ParentID != nil {
			parent = *artifact.ParentID
		}
		fetched := artifact.FetchedAt.UnixNano()
		if artifact.FetchedAt.IsZero() {
			fetched = time.Now().UTC().UnixNano()
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO changelog_artifacts(url,media_type,etag,last_modified,content_hash,discovery_parent,fetched_at,raw_content,extracted_text,extraction_status)
			VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(url,content_hash) DO NOTHING`, artifact.URL, artifact.MediaType, artifact.ETag, artifact.LastModified, artifact.Hash, parent, fetched, artifact.Raw, artifact.Extracted, "success")
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM changelog_artifacts WHERE url=? AND content_hash=?`, artifact.URL, artifact.Hash).Scan(&id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_changelog_artifacts(package_id,artifact_id) VALUES(?,?) ON CONFLICT DO NOTHING`, packageID, id)
		return err
	})
	if err != nil {
		return ChangelogArtifact{}, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM changelog_artifacts WHERE url=? AND content_hash=?`, artifact.URL, artifact.Hash).Scan(&id)
	artifact.ID = strconv.FormatInt(id, 10)
	return artifact, err
}

func (s *Store) ChangelogArtifacts(ctx context.Context, packageID string) ([]ChangelogArtifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.url,a.media_type,a.etag,a.last_modified,a.content_hash,a.discovery_parent,a.fetched_at,a.raw_content,a.extracted_text FROM changelog_artifacts a JOIN package_changelog_artifacts p ON p.artifact_id=a.id WHERE p.package_id=? ORDER BY a.fetched_at DESC,a.id DESC`, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ChangelogArtifact
	for rows.Next() {
		var a ChangelogArtifact
		var id, fetched int64
		var parent string
		if err := rows.Scan(&id, &a.URL, &a.MediaType, &a.ETag, &a.LastModified, &a.Hash, &parent, &fetched, &a.Raw, &a.Extracted); err != nil {
			return nil, err
		}
		a.ID = strconv.FormatInt(id, 10)
		a.FetchedAt = time.Unix(0, fetched).UTC()
		if parent != "" {
			a.ParentID = &parent
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) SaveChangelogSection(ctx context.Context, section ChangelogSection) error {
	id, err := strconv.ParseInt(section.ArtifactID, 10, 64)
	if err != nil {
		return err
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO changelog_sections(artifact_id,version,content,confidence,source_url)
			SELECT ?,?,?,?,?
			WHERE NOT EXISTS (
				SELECT 1 FROM changelog_sections
				WHERE artifact_id=? AND version=? AND content=? AND confidence=? AND source_url=?
			)`,
			id, section.Version, section.Body, section.Confidence, section.SourceURL,
			id, section.Version, section.Body, section.Confidence, section.SourceURL)
		return err
	})
}

func (s *Store) ChangelogSections(ctx context.Context, artifactID string) ([]ChangelogSection, error) {
	id, err := strconv.ParseInt(artifactID, 10, 64)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version,content,confidence,source_url FROM changelog_sections WHERE artifact_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ChangelogSection
	for rows.Next() {
		var v ChangelogSection
		v.ArtifactID = artifactID
		if err := rows.Scan(&v.Version, &v.Body, &v.Confidence, &v.SourceURL); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

func (s *Store) RecordChangelogFailure(ctx context.Context, url string, at time.Time, failure error) error {
	if failure == nil {
		return errors.New("changelog failure is nil")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		var id sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT id FROM changelog_artifacts WHERE url=? ORDER BY fetched_at DESC,id DESC LIMIT 1`, url).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO changelog_attempts(artifact_id,url,attempted_at,status,error) VALUES(?,?,?,?,?)`, id, url, at.UnixNano(), "failed", failure.Error())
		return err
	})
}

func (s *Store) ChangelogRetryAfter(ctx context.Context, url string) (time.Time, error) {
	var attempted int64
	err := s.db.QueryRowContext(ctx, `SELECT attempted_at FROM changelog_attempts WHERE url=? ORDER BY attempted_at DESC LIMIT 1`, url).Scan(&attempted)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, attempted).UTC().Add(time.Minute), nil
}
