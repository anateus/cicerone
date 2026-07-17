CREATE TABLE history_aliases (
  repository TEXT NOT NULL,
  alias TEXT NOT NULL,
  package_id TEXT NOT NULL,
  commit_hash TEXT NOT NULL,
  PRIMARY KEY(repository, alias)
);
CREATE INDEX history_aliases_commit ON history_aliases(repository, commit_hash);
CREATE TABLE history_diagnostics (
  repository TEXT NOT NULL,
  commit_hash TEXT NOT NULL,
  definition_path TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL,
  PRIMARY KEY(repository, commit_hash, definition_path, message)
);
CREATE INDEX history_diagnostics_commit ON history_diagnostics(repository, commit_hash);
