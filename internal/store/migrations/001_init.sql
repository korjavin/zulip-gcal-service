-- Times are unix seconds (UTC). Booleans are 0/1.

CREATE TABLE accounts (
    id                  TEXT PRIMARY KEY,           -- random, never reused
    google_sub          TEXT NOT NULL UNIQUE,
    google_email        TEXT NOT NULL,
    email_authoritative INTEGER NOT NULL DEFAULT 0,
    enc_refresh_token   BLOB,
    token_rev           INTEGER NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'connected' CHECK (status IN ('connected', 'disconnected')),
    zulip_user_id       INTEGER UNIQUE,
    linked_at           INTEGER,
    paused              INTEGER NOT NULL DEFAULT 0,
    schedule_rev        INTEGER NOT NULL DEFAULT 0,
    last_poll_ok_at     INTEGER,
    created_at          INTEGER NOT NULL
);

CREATE TABLE settings (
    account_id    TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    timing        TEXT NOT NULL DEFAULT 'google' CHECK (timing IN ('google', 'fixed')),
    lead_minutes  INTEGER NOT NULL CHECK (lead_minutes BETWEEN 1 AND 60),
    skip_declined INTEGER NOT NULL DEFAULT 1,
    calendars     TEXT NOT NULL DEFAULT '["primary"]'  -- JSON array, ordered
);

CREATE TABLE reminders (
    key              TEXT PRIMARY KEY,                 -- account|iCalUID|startUTC|offset
    account_id       TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    fire_at          INTEGER NOT NULL,
    event_start      INTEGER NOT NULL,
    payload          TEXT NOT NULL,                    -- JSON
    state            TEXT NOT NULL CHECK (state IN ('pending', 'sending', 'sent', 'expired', 'skipped')),
    zulip_message_id INTEGER,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX reminders_account ON reminders(account_id);
CREATE INDEX reminders_due ON reminders(state, fire_at);

CREATE TABLE link_codes (
    code       TEXT PRIMARY KEY,
    account_id TEXT NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL
);

-- Not tied to accounts (design §3.4); purged after 7 days.
CREATE TABLE handled_messages (
    message_id INTEGER PRIMARY KEY,
    handled_at INTEGER NOT NULL
);

-- Disconnected Google subs (design §3.3); no FK, purged after 1 hour.
CREATE TABLE tombstones (
    google_sub TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    revoking   INTEGER NOT NULL DEFAULT 0
);
