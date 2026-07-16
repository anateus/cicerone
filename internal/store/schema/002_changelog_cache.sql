DROP TRIGGER IF EXISTS changelog_artifacts_ai;
DROP TRIGGER IF EXISTS changelog_artifacts_ad;
DROP TRIGGER IF EXISTS changelog_artifacts_au;

CREATE TABLE changelog_artifacts_v2 (
  id INTEGER PRIMARY KEY,
  url TEXT NOT NULL,
  media_type TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  last_modified TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  discovery_parent TEXT NOT NULL DEFAULT '',
  fetched_at INTEGER NOT NULL DEFAULT 0,
  raw_content TEXT NOT NULL DEFAULT '',
  extracted_text TEXT NOT NULL DEFAULT '',
  extraction_status TEXT NOT NULL DEFAULT '',
  UNIQUE(url, content_hash)
);
INSERT INTO changelog_artifacts_v2(id,url,media_type,etag,last_modified,content_hash,discovery_parent,fetched_at,raw_content,extracted_text,extraction_status)
SELECT id,url,media_type,etag,last_modified,content_hash,discovery_parent,fetched_at,raw_content,extracted_text,extraction_status FROM changelog_artifacts;

CREATE TABLE package_changelog_artifacts (
  package_id TEXT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  artifact_id INTEGER NOT NULL REFERENCES changelog_artifacts_v2(id) ON DELETE CASCADE,
  PRIMARY KEY(package_id, artifact_id)
);
INSERT INTO package_changelog_artifacts(package_id,artifact_id)
SELECT package_id,id FROM changelog_artifacts WHERE package_id IS NOT NULL;

CREATE TABLE changelog_sections_v2 (
  id INTEGER PRIMARY KEY,
  artifact_id INTEGER NOT NULL REFERENCES changelog_artifacts_v2(id) ON DELETE CASCADE,
  version TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 0,
  source_url TEXT NOT NULL DEFAULT ''
);
INSERT INTO changelog_sections_v2(id,artifact_id,version,content)
SELECT id,artifact_id,version,content FROM changelog_sections;

CREATE TABLE changelog_attempts_v2 (
  id INTEGER PRIMARY KEY,
  artifact_id INTEGER REFERENCES changelog_artifacts_v2(id) ON DELETE CASCADE,
  url TEXT NOT NULL DEFAULT '',
  attempted_at INTEGER NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);
INSERT INTO changelog_attempts_v2(id,artifact_id,url,attempted_at,status,error)
SELECT a.id,a.artifact_id,COALESCE(c.url,''),a.attempted_at,a.status,a.error
FROM changelog_attempts a LEFT JOIN changelog_artifacts c ON c.id=a.artifact_id;

DROP TABLE changelog_sections;
DROP TABLE changelog_attempts;
DROP TABLE changelog_artifacts;
ALTER TABLE changelog_artifacts_v2 RENAME TO changelog_artifacts;
ALTER TABLE changelog_sections_v2 RENAME TO changelog_sections;
ALTER TABLE changelog_attempts_v2 RENAME TO changelog_attempts;

CREATE VIRTUAL TABLE IF NOT EXISTS changelog_artifacts_fts USING fts5(extracted_text, content='changelog_artifacts', content_rowid='id');
INSERT INTO changelog_artifacts_fts(changelog_artifacts_fts) VALUES('rebuild');
CREATE TRIGGER changelog_artifacts_ai AFTER INSERT ON changelog_artifacts BEGIN
  INSERT INTO changelog_artifacts_fts(rowid, extracted_text) VALUES (new.id, new.extracted_text);
END;
CREATE TRIGGER changelog_artifacts_ad AFTER DELETE ON changelog_artifacts BEGIN
  INSERT INTO changelog_artifacts_fts(changelog_artifacts_fts, rowid, extracted_text) VALUES ('delete', old.id, old.extracted_text);
END;
CREATE TRIGGER changelog_artifacts_au AFTER UPDATE ON changelog_artifacts BEGIN
  INSERT INTO changelog_artifacts_fts(changelog_artifacts_fts, rowid, extracted_text) VALUES ('delete', old.id, old.extracted_text);
  INSERT INTO changelog_artifacts_fts(rowid, extracted_text) VALUES (new.id, new.extracted_text);
END;
