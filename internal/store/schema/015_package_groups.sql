CREATE TABLE package_groups (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  position INTEGER NOT NULL DEFAULT 0
);
-- Add group membership. The status CHECK from migration 014 still names
-- 'snoozed', so hidden packages keep using the legacy 'snoozed' value on disk
-- and the store layer maps it to the hidden group scope.
ALTER TABLE packages ADD COLUMN group_id INTEGER REFERENCES package_groups(id) ON DELETE SET NULL;
