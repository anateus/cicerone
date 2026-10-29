CREATE TABLE history_scan_cursors (
  repository TEXT NOT NULL,
  scan_key TEXT NOT NULL,
  head_commit TEXT NOT NULL,
  frontier_commit TEXT NOT NULL,
  range_floor TEXT NOT NULL,
  since TEXT NOT NULL,
  until TEXT NOT NULL,
  fallback_since TEXT NOT NULL,
  PRIMARY KEY(repository, scan_key)
);
