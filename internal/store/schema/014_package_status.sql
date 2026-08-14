ALTER TABLE packages ADD COLUMN status TEXT NOT NULL DEFAULT 'default'
  CHECK(status IN ('default', 'starred', 'snoozed'));
