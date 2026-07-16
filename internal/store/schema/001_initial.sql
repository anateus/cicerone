CREATE TABLE repositories (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL DEFAULT '',
  head_commit TEXT NOT NULL DEFAULT ''
);
CREATE TABLE repository_ranges (
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  start_commit TEXT NOT NULL,
  end_commit TEXT NOT NULL,
  PRIMARY KEY(repository_id, start_commit, end_commit)
);
CREATE TABLE sync_runs (
  id INTEGER PRIMARY KEY,
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  started_at INTEGER NOT NULL,
  completed_at INTEGER,
  error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE packages (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  type TEXT NOT NULL CHECK(type IN ('formula', 'cask'))
);
CREATE TABLE package_aliases (
  alias TEXT PRIMARY KEY,
  package_id TEXT NOT NULL REFERENCES packages(id) ON DELETE CASCADE
);
CREATE TABLE update_events (
  id TEXT PRIMARY KEY,
  package_id TEXT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('version', 'revision', 'metadata')),
  old_version TEXT NOT NULL DEFAULT '',
  new_version TEXT NOT NULL DEFAULT '',
  old_revision TEXT NOT NULL DEFAULT '',
  new_revision TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL,
  definition_path TEXT NOT NULL DEFAULT '',
  commit_hash TEXT NOT NULL,
  event_time INTEGER NOT NULL,
  diagnostic TEXT NOT NULL DEFAULT '',
  UNIQUE(repository, commit_hash, package_id, kind)
);
CREATE INDEX update_events_time ON update_events(event_time DESC, id);
CREATE INDEX update_events_package_kind_time ON update_events(package_id, kind, event_time DESC);
CREATE TABLE installed_packages (
  package_id TEXT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  type TEXT NOT NULL CHECK(type IN ('formula', 'cask')),
  pinned INTEGER NOT NULL CHECK(pinned IN (0, 1)),
  upgrade_available INTEGER NOT NULL CHECK(upgrade_available IN (0, 1))
);
CREATE UNIQUE INDEX installed_packages_identity ON installed_packages(package_id, type);
CREATE TABLE changelog_artifacts (
  id INTEGER PRIMARY KEY,
  package_id TEXT REFERENCES packages(id) ON DELETE CASCADE,
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
CREATE TABLE changelog_sections (
  id INTEGER PRIMARY KEY,
  artifact_id INTEGER NOT NULL REFERENCES changelog_artifacts(id) ON DELETE CASCADE,
  version TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 0,
  source_url TEXT NOT NULL DEFAULT ''
);
CREATE TABLE changelog_attempts (
  id INTEGER PRIMARY KEY,
  artifact_id INTEGER REFERENCES changelog_artifacts(id) ON DELETE CASCADE,
  url TEXT NOT NULL DEFAULT '',
  attempted_at INTEGER NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE preferences (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  horizon_seconds INTEGER NOT NULL,
  show_version INTEGER NOT NULL CHECK(show_version IN (0, 1)),
  show_revision INTEGER NOT NULL CHECK(show_revision IN (0, 1)),
  show_metadata INTEGER NOT NULL CHECK(show_metadata IN (0, 1)),
  show_formula INTEGER NOT NULL CHECK(show_formula IN (0, 1)),
  show_cask INTEGER NOT NULL CHECK(show_cask IN (0, 1)),
  query TEXT NOT NULL,
  roll_up INTEGER NOT NULL CHECK(roll_up IN (0, 1))
);
INSERT INTO preferences VALUES (1, 2592000, 1, 0, 0, 1, 1, '', 1);

CREATE VIRTUAL TABLE packages_fts USING fts5(name, content='packages', content_rowid='rowid');
CREATE TRIGGER packages_ai AFTER INSERT ON packages BEGIN
  INSERT INTO packages_fts(rowid, name) VALUES (new.rowid, new.name);
END;
CREATE TRIGGER packages_ad AFTER DELETE ON packages BEGIN
  INSERT INTO packages_fts(packages_fts, rowid, name) VALUES ('delete', old.rowid, old.name);
END;
CREATE TRIGGER packages_au AFTER UPDATE ON packages BEGIN
  INSERT INTO packages_fts(packages_fts, rowid, name) VALUES ('delete', old.rowid, old.name);
  INSERT INTO packages_fts(rowid, name) VALUES (new.rowid, new.name);
END;
CREATE VIRTUAL TABLE changelog_artifacts_fts USING fts5(extracted_text, content='changelog_artifacts', content_rowid='id');
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
