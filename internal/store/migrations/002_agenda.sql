-- Daily agenda DM (design §3.4), off by default.
ALTER TABLE settings ADD COLUMN agenda_minute   INTEGER CHECK (agenda_minute BETWEEN 0 AND 1439); -- local minute of day; NULL = off
ALTER TABLE settings ADD COLUMN agenda_workdays INTEGER NOT NULL DEFAULT 0;  -- Mon-Fri only
ALTER TABLE settings ADD COLUMN agenda_tz       TEXT NOT NULL DEFAULT '';    -- IANA name from the Zulip profile; '' = resolve
ALTER TABLE settings ADD COLUMN agenda_sent     TEXT NOT NULL DEFAULT '';    -- local date (YYYY-MM-DD) of the last agenda
