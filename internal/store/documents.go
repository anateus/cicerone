package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type DocumentKind string

const (
	DocumentREADME    DocumentKind = "readme"
	DocumentChangelog DocumentKind = "changelog"
)

type PackageDocument struct {
	ID, URL, SourceURL, MediaType, ETag, LastModified, Hash string
	ParentID, ExtractionStatus                              string
	Kind                                                    DocumentKind
	FetchedAt                                               time.Time
	Raw, Extracted                                          []byte
}

type DocumentSection struct {
	DocumentID, Version, Body, SourceURL string
	Confidence                           float64
}

type PackageInfoRecord struct {
	PackageID   string
	FetchedAt   time.Time
	Raw         []byte
	Normalized  []byte
	Description string
}

func (s *Store) SavePackageInfo(ctx context.Context, record PackageInfoRecord) error {
	if record.Description == "" && len(record.Normalized) > 0 {
		var normalized struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(record.Normalized, &normalized); err != nil {
			return fmt.Errorf("decode normalized package info: %w", err)
		}
		record.Description = normalized.Description
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO package_info(package_id,fetched_at,raw_json,normalized_json,description)
				VALUES(?,?,?,?,?) ON CONFLICT(package_id) DO UPDATE SET
				fetched_at=excluded.fetched_at,raw_json=excluded.raw_json,
				normalized_json=excluded.normalized_json,description=excluded.description`,
			record.PackageID, record.FetchedAt.UnixNano(), record.Raw, record.Normalized, record.Description)
		return err
	})
}

func (s *Store) PackageInfo(ctx context.Context, packageID string) (PackageInfoRecord, bool, error) {
	var record PackageInfoRecord
	var fetched int64
	err := s.db.QueryRowContext(ctx, `SELECT package_id,fetched_at,raw_json,normalized_json,description
			FROM package_info WHERE package_id=?`, packageID).
		Scan(&record.PackageID, &fetched, &record.Raw, &record.Normalized, &record.Description)
	if err == sql.ErrNoRows {
		return PackageInfoRecord{}, false, nil
	}
	if err != nil {
		return PackageInfoRecord{}, false, err
	}
	record.FetchedAt = time.Unix(0, fetched).UTC()
	return record, true, nil
}

func (s *Store) PackageDocuments(ctx context.Context, packageID string, kind DocumentKind) ([]PackageDocument, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.kind,d.canonical_url,d.source_url,d.media_type,d.etag,d.last_modified,
		d.content_hash,d.discovery_parent,d.fetched_at,d.raw_content,d.extracted_text,d.extraction_status
		FROM package_documents d
		JOIN package_document_links l ON l.document_id=d.id
		WHERE l.package_id=? AND l.kind=?
		ORDER BY d.fetched_at DESC,d.id DESC`, packageID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PackageDocument
	for rows.Next() {
		var document PackageDocument
		var id int64
		var fetched int64
		if err := rows.Scan(&id, &document.Kind, &document.URL, &document.SourceURL, &document.MediaType,
			&document.ETag, &document.LastModified, &document.Hash, &document.ParentID, &fetched,
			&document.Raw, &document.Extracted, &document.ExtractionStatus); err != nil {
			return nil, err
		}
		document.ID = strconv.FormatInt(id, 10)
		if fetched != 0 {
			document.FetchedAt = time.Unix(0, fetched).UTC()
		}
		result = append(result, document)
	}
	return result, rows.Err()
}

func (s *Store) DocumentSections(ctx context.Context, documentID string) ([]DocumentSection, error) {
	id, err := strconv.ParseInt(documentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid document ID %q", documentID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT document_id,version,content,confidence,source_url
		FROM document_sections WHERE document_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DocumentSection
	for rows.Next() {
		var section DocumentSection
		var scannedID int64
		if err := rows.Scan(&scannedID, &section.Version, &section.Body, &section.Confidence, &section.SourceURL); err != nil {
			return nil, err
		}
		section.DocumentID = strconv.FormatInt(scannedID, 10)
		result = append(result, section)
	}
	return result, rows.Err()
}

func (s *Store) SavePackageDocument(ctx context.Context, packageID string, document PackageDocument) (PackageDocument, error) {
	var id int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO package_documents(
			kind,canonical_url,source_url,media_type,etag,last_modified,content_hash,discovery_parent,
			fetched_at,raw_content,extracted_text,extraction_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(kind,canonical_url,content_hash) DO NOTHING`,
			document.Kind, document.URL, document.SourceURL, document.MediaType, document.ETag,
			document.LastModified, document.Hash, document.ParentID, document.FetchedAt.UnixNano(),
			document.Raw, document.Extracted, document.ExtractionStatus)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM package_documents
			WHERE kind=? AND canonical_url=? AND content_hash=?`,
			document.Kind, document.URL, document.Hash).Scan(&id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO package_document_links(package_id,document_id,kind)
			VALUES(?,?,?) ON CONFLICT DO NOTHING`, packageID, id, document.Kind)
		return err
	})
	if err != nil {
		return PackageDocument{}, err
	}
	document.ID = strconv.FormatInt(id, 10)
	return document, nil
}

func (s *Store) TouchPackageDocument(ctx context.Context, documentID string, fetchedAt time.Time, etag, lastModified string) error {
	id, err := strconv.ParseInt(documentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid document ID %q", documentID)
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE package_documents SET fetched_at=?,
			etag=CASE WHEN ?='' THEN etag ELSE ? END,
			last_modified=CASE WHEN ?='' THEN last_modified ELSE ? END WHERE id=?`,
			fetchedAt.UnixNano(), etag, etag, lastModified, lastModified, id)
		return err
	})
}
