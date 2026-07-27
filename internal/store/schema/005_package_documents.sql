CREATE TABLE package_documents (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL CHECK(kind IN ('readme', 'changelog')),
  canonical_url TEXT NOT NULL,
  source_url TEXT NOT NULL,
  media_type TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  last_modified TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  discovery_parent TEXT NOT NULL DEFAULT '',
  fetched_at INTEGER NOT NULL DEFAULT 0,
  raw_content TEXT NOT NULL DEFAULT '',
  extracted_text TEXT NOT NULL DEFAULT '',
  extraction_status TEXT NOT NULL DEFAULT '',
  UNIQUE(kind, canonical_url, content_hash)
);

CREATE TABLE package_document_links (
  package_id TEXT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  document_id INTEGER NOT NULL REFERENCES package_documents(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('readme', 'changelog')),
  priority INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(package_id, document_id, kind)
);

CREATE TABLE document_sections (
  id INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES package_documents(id) ON DELETE CASCADE,
  version TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 0,
  source_url TEXT NOT NULL DEFAULT ''
);

CREATE TABLE document_attempts (
  id INTEGER PRIMARY KEY,
  document_id INTEGER REFERENCES package_documents(id) ON DELETE CASCADE,
  canonical_url TEXT NOT NULL,
  attempted_at INTEGER NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE package_info (
  package_id TEXT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
  fetched_at INTEGER NOT NULL,
  raw_json TEXT NOT NULL,
  normalized_json TEXT NOT NULL
);

INSERT INTO package_documents(
  id,kind,canonical_url,source_url,media_type,etag,last_modified,content_hash,
  discovery_parent,fetched_at,raw_content,extracted_text,extraction_status
)
SELECT id,'changelog',url,url,media_type,etag,last_modified,content_hash,
       discovery_parent,fetched_at,raw_content,extracted_text,extraction_status
FROM changelog_artifacts;

INSERT INTO package_document_links(package_id,document_id,kind)
SELECT package_id,artifact_id,'changelog' FROM package_changelog_artifacts;

INSERT INTO document_sections(id,document_id,version,content,confidence,source_url)
SELECT id,artifact_id,version,content,confidence,source_url FROM changelog_sections;

INSERT INTO document_attempts(id,document_id,canonical_url,attempted_at,status,error)
SELECT id,artifact_id,url,attempted_at,status,error FROM changelog_attempts;

CREATE VIRTUAL TABLE package_documents_fts USING fts5(
  extracted_text,
  content='package_documents',
  content_rowid='id'
);
INSERT INTO package_documents_fts(package_documents_fts) VALUES('rebuild');
CREATE TRIGGER package_documents_ai AFTER INSERT ON package_documents BEGIN
  INSERT INTO package_documents_fts(rowid, extracted_text) VALUES (new.id, new.extracted_text);
END;
CREATE TRIGGER package_documents_ad AFTER DELETE ON package_documents BEGIN
  INSERT INTO package_documents_fts(package_documents_fts, rowid, extracted_text)
  VALUES ('delete', old.id, old.extracted_text);
END;
CREATE TRIGGER package_documents_au AFTER UPDATE ON package_documents BEGIN
  INSERT INTO package_documents_fts(package_documents_fts, rowid, extracted_text)
  VALUES ('delete', old.id, old.extracted_text);
  INSERT INTO package_documents_fts(rowid, extracted_text) VALUES (new.id, new.extracted_text);
END;
