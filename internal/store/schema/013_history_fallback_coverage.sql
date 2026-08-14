CREATE TABLE history_fallback_coverage (
  repository TEXT NOT NULL,
  package_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  PRIMARY KEY(repository, package_id, kind)
);
