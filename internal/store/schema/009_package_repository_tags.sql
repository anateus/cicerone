CREATE TABLE package_repository_tags (
  package_id TEXT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
  tags_json TEXT NOT NULL DEFAULT '[]',
  fetched_at INTEGER NOT NULL
);
