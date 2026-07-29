ALTER TABLE preferences
ADD COLUMN search_scope TEXT NOT NULL DEFAULT 'names'
CHECK(search_scope IN ('names', 'descriptions', 'changelogs', 'readmes'));

ALTER TABLE package_info
ADD COLUMN description TEXT NOT NULL DEFAULT '';

UPDATE package_info
SET description = CASE
  WHEN json_valid(normalized_json)
  THEN COALESCE(json_extract(normalized_json, '$.description'), '')
  ELSE ''
END;

CREATE VIRTUAL TABLE package_info_fts USING fts5(
  description,
  content='package_info',
  content_rowid='rowid'
);
INSERT INTO package_info_fts(package_info_fts) VALUES('rebuild');
CREATE TRIGGER package_info_ai AFTER INSERT ON package_info BEGIN
  INSERT INTO package_info_fts(rowid, description) VALUES (new.rowid, new.description);
END;
CREATE TRIGGER package_info_ad AFTER DELETE ON package_info BEGIN
  INSERT INTO package_info_fts(package_info_fts, rowid, description)
  VALUES ('delete', old.rowid, old.description);
END;
CREATE TRIGGER package_info_au AFTER UPDATE ON package_info BEGIN
  INSERT INTO package_info_fts(package_info_fts, rowid, description)
  VALUES ('delete', old.rowid, old.description);
  INSERT INTO package_info_fts(rowid, description) VALUES (new.rowid, new.description);
END;
