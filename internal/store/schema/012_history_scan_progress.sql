CREATE TABLE history_scan_progress (
  repository TEXT NOT NULL,
  scan_key TEXT NOT NULL,
  commit_hash TEXT NOT NULL,
  event_count INTEGER NOT NULL DEFAULT 0,
  diagnostic_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(repository, scan_key, commit_hash)
);
