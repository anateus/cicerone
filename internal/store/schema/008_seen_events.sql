ALTER TABLE update_events
ADD COLUMN seen INTEGER NOT NULL DEFAULT 0 CHECK(seen IN (0, 1));

-- Events that predate this feature are the user's initial acknowledged baseline.
UPDATE update_events SET seen = 1;
