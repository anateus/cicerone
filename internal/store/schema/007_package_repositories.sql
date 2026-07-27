CREATE TABLE package_repositories (
  package_id TEXT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
  canonical_url TEXT NOT NULL,
  source_url TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 0,
  discovered_at INTEGER NOT NULL
);
