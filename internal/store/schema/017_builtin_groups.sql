-- Starred and Hidden become ordinary package groups. Membership moves from
-- the packages.status column to packages.group_id, and kind tells the built-in
-- groups apart so the feed can keep hidden packages out of All.
ALTER TABLE package_groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'
  CHECK(kind IN ('user', 'starred', 'hidden'));

-- Free the built-in names if a user group already took one.
UPDATE package_groups SET name = name || ' (custom)' WHERE name IN ('Starred', 'Hidden');

INSERT INTO package_groups(name, position, kind) VALUES ('Starred', 0, 'starred'), ('Hidden', 0, 'hidden');

-- A package belongs to one group. Hidden wins because it already removed the
-- package from every view; a star only moves packages not in a user group.
UPDATE packages SET group_id = (SELECT id FROM package_groups WHERE kind = 'hidden')
  WHERE status = 'snoozed';
UPDATE packages SET group_id = (SELECT id FROM package_groups WHERE kind = 'starred')
  WHERE status = 'starred' AND group_id IS NULL;

-- Saved Starred (2) and Hidden (3) tabs become user-group scope (4).
UPDATE preferences SET group_target = (SELECT id FROM package_groups WHERE kind = 'starred'), group_scope = 4
  WHERE group_scope = 2;
UPDATE preferences SET group_target = (SELECT id FROM package_groups WHERE kind = 'hidden'), group_scope = 4
  WHERE group_scope = 3;

ALTER TABLE packages DROP COLUMN status;
