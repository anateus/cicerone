package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type PackageRepository struct {
	PackageID, URL, SourceURL string
	Confidence                float64
	DiscoveredAt              time.Time
}

type PackageRepositoryTags struct {
	PackageID string
	Tags      []string
	FetchedAt time.Time
}

func (s *Store) SavePackageRepository(ctx context.Context, repository PackageRepository) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO package_repositories(
			package_id,canonical_url,source_url,confidence,discovered_at) VALUES(?,?,?,?,?)
			ON CONFLICT(package_id) DO UPDATE SET canonical_url=excluded.canonical_url,
			source_url=excluded.source_url,confidence=excluded.confidence,discovered_at=excluded.discovered_at`,
			repository.PackageID, repository.URL, repository.SourceURL, repository.Confidence, repository.DiscoveredAt.UnixNano())
		return err
	})
}

func (s *Store) PackageRepository(ctx context.Context, packageID string) (PackageRepository, bool, error) {
	var repository PackageRepository
	var discoveredAt int64
	err := s.db.QueryRowContext(ctx, `SELECT package_id,canonical_url,source_url,confidence,discovered_at
		FROM package_repositories WHERE package_id=?`, packageID).Scan(
		&repository.PackageID, &repository.URL, &repository.SourceURL, &repository.Confidence, &discoveredAt)
	if err == sql.ErrNoRows {
		return PackageRepository{}, false, nil
	}
	if err != nil {
		return PackageRepository{}, false, err
	}
	repository.DiscoveredAt = time.Unix(0, discoveredAt).UTC()
	return repository, true, nil
}

func (s *Store) SavePackageRepositoryTags(ctx context.Context, record PackageRepositoryTags) error {
	encoded, err := json.Marshal(record.Tags)
	if err != nil {
		return err
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO package_repository_tags(package_id,tags_json,fetched_at)
			VALUES(?,?,?) ON CONFLICT(package_id) DO UPDATE SET
			tags_json=excluded.tags_json,fetched_at=excluded.fetched_at`,
			record.PackageID, encoded, record.FetchedAt.UnixNano())
		return err
	})
}

func (s *Store) PackageRepositoryTags(ctx context.Context, packageID string) (PackageRepositoryTags, bool, error) {
	record := PackageRepositoryTags{PackageID: packageID}
	var encoded []byte
	var fetchedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT tags_json,fetched_at FROM package_repository_tags
		WHERE package_id=?`, packageID).Scan(&encoded, &fetchedAt)
	if err == sql.ErrNoRows {
		return PackageRepositoryTags{}, false, nil
	}
	if err != nil {
		return PackageRepositoryTags{}, false, err
	}
	if err := json.Unmarshal(encoded, &record.Tags); err != nil {
		return PackageRepositoryTags{}, false, err
	}
	if record.Tags == nil {
		record.Tags = []string{}
	}
	record.FetchedAt = time.Unix(0, fetchedAt).UTC()
	return record, true, nil
}
