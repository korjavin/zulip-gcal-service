# zulip-gcal-service: design

Google Calendar reminders in Zulip, for everyone in the organization, without
anybody running a script. One self-hosted HTTPS service, one Zulip bot, one
Google OAuth client. A user clicks **Sign in with Google** once and from then on
gets a Zulip DM before each meeting.

Replaces Zulip's stock Google Calendar integration, which asks every user to
install Python, download `client_secret.json` and a `zuliprc`, and keep a
terminal (or `screen`) open forever.

Requires Zulip Server 10+ (feature level 302+) and a Google Workspace
organization (see §2).

## 1. What a user does

1. Opens `https://calendar.example.com` and clicks **Sign in with Google**.
   Google asks for read-only access to their calendar.
2. Done. The page says "Connected — reminders go to you in Zulip as DMs from
   *Calendar*", and the bot sends a welcome DM.

If the service cannot match the user's Zulip account by their Google e-mail
(different addresses, e-mail hidden by the realm's visibility settings, or a
Google e-mail that is not authoritative — see §3), the page shows a short code
and a button **Open Zulip**, which opens a one-to-one DM with the bot. The user
sends the code; the bot links that Zulip account and replies. Zulip identifies
the sender, so no e-mail lookup is needed.

One Google account per Zulip user. To link a different one: Disconnect, then
sign in again.

Everything else is optional, on the settings page (same Google sign-in):
reminder timing, which calendars, skip declined events, pause, disconnect,
send a test reminder.

Without leaving Zulip, the user can DM the bot:

* `stop` — pause reminders (calendar access kept); `start` resumes.
* `disconnect` — delete everything this service stores about the user,
  refresh token included, and revoke the Google access. Signing in again on
  the page reconnects.

The bot always replies; no reply within a minute → send again or use the
website.

## 2. What an admin does (once)

1. Google Cloud: enable the Calendar API, create an OAuth client (Web
   application), redirect URI `https://calendar.example.com/oauth/callback`,
   scopes `openid email calendar.readonly`. Make the consent screen
   **Internal**: no Google verification, no 7-day refresh-token expiry of
   *External/Testing* apps, only the organization's accounts can sign in.
   (Tokens can still be invalidated by revocation, long inactivity or admin
   policy; the service handles that, §3.) *External/Production* works too but
   needs Google's verification for the sensitive calendar scope — not
   documented here.
2. Zulip: create a **Generic bot** named "Calendar", copy its e-mail and API
   key.
3. Deploy `docker-compose.yml` (Portainer stack or plain compose) with the
   env below, behind any TLS reverse proxy (Traefik labels included). Run
   exactly **one** instance (the scheduler is not safe to run twice).

| env | required | meaning |
|---|---|---|
| `PUBLIC_URL` | yes | `https://calendar.example.com`, used for redirect URI and links |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | yes | OAuth client |
| `ZULIP_SITE`, `ZULIP_BOT_EMAIL`, `ZULIP_BOT_API_KEY` | yes | the Calendar bot |
| `APP_SECRET` | yes | 32+ random bytes, base64; subkeys for token encryption and cookies are derived from it (HKDF) |
| `ALLOWED_DOMAINS` | no | comma-separated Workspace domains, checked against the ID token's `hd` claim |
| `DATA_DIR` | no | default `/data` (SQLite file lives here; mount a volume) |
| `DEFAULT_LEAD_MINUTES` | no | default 10, allowed 1–60 |
| `POLL_INTERVAL` | no | default `3m` |

## 3. Architecture

One Go binary, one SQLite file (`modernc.org/sqlite`, no CGO), no other
services, exactly one running instance.

```
browser ──HTTPS──> [web]  Google sign-in, settings pages
                     │
                  [SQLite]  accounts, encrypted refresh tokens, settings,
                     │      reminders, link codes, handled bot messages
                     │
 Google Calendar <─[poller]  every POLL_INTERVAL per account: events in the
                     │       next 26h → reconcile reminders
                     │
                  [sender]   every 30s: due reminders → Zulip DM
                     │
 Zulip <──────────[bot]      send DMs; long-poll the bot's event queue for
                             new one-to-one DMs (link codes, commands)
```

### 3.1 Concurrency: no locks, conditional writes

All HTTP (Google, Zulip) happens outside DB transactions. Every state change
is a short SQLite write whose `WHERE` clause checks the state it was based on;
SQLite serializes writers, so that check is the whole concurrency story.

* `accounts.schedule_rev` — bumped by anything that changes which reminders an
  account should have: settings save, pause/resume, link/unlink. A poll reads
  it before fetching and commits only `WHERE schedule_rev = <read value>`
  and the account is still connected, linked and not paused; otherwise its
  result is thrown away and the account is re-polled.
* `accounts.token_rev` — bumped when a new refresh token is stored. A refresh
  failure marks the account disconnected only `WHERE token_rev = <rev used>`,
  so a stale failure never kills a newer grant.
* Schedule-affecting changes (save, pause, link, unlink, lost access) bump
  `schedule_rev` and delete the account's `pending` and `sending` reminders in
  the same write, so they apply even while Google is unreachable; the next
  poll rebuilds them.
* At most one poll per account in flight (in-memory set); an on-demand poll
  for an account already being polled is coalesced into one re-poll.
* The sender claims a reminder with one conditional update
  (`pending → sending` only if the row is still pending and its account is
  connected, linked and not paused), then sends. Anything that commits before
  the claim wins; the one HTTP request already in flight may still deliver
  after a `stop`, nothing after it. The sender's follow-up writes (`sent`,
  back to `pending` on a temporary error, delete on a permanent one) all
  require the row to still be `sending`, so a row cancelled meanwhile is
  never revived. On startup leftover `sending` rows go back to `pending`
  (possible duplicate, never a loss).

### 3.2 Accounts, sessions, tokens

* Accounts have a random, never-reused id (not an SQLite rowid), keyed by
  Google `sub`.
* **Session**: signed `HttpOnly; Secure; SameSite=Lax` cookie with the account
  id and expiry, checked on every request that the account still exists.
  Deleting the account logs out every browser; settings changes and pause do
  not. A disconnected (lost Google access) account still has a session, so
  its page can show **Reconnect**. Settings forms carry a CSRF token.
* **Tokens at rest**: refresh tokens AES-256-GCM encrypted with a key derived
  from `APP_SECRET`. Never logged. Access tokens only in memory, cached per
  account by `token_rev`.
* **OAuth**: authorization-code flow, `access_type=offline`, `state` + PKCE.
  `/login` starts without forced consent; the callback learns the Google `sub`,
  and if that account has no usable refresh token (new, or the stored one is
  dead), it retries once with `prompt=consent`. A failed callback (missing
  calendar scope, no refresh token after the retry) never overwrites a working
  stored connection. Granted scopes are checked in the token response.
* **Identity**: the Google e-mail is used for Zulip auto-match only when
  `email_verified` and authoritative (`hd` present, or a `gmail.com` address);
  otherwise the DM-code path.
* **Google project**: use a dedicated Cloud project for this service —
  revoking a token revokes the user's grant to the whole OAuth client.

### 3.3 Lifecycle operations

One implementation each, used by the web pages and by bot commands alike.

* **Pause / resume**: flip `paused`, bump `schedule_rev`, delete pending
  reminders; resume also triggers a poll. Repeating the current state is a
  no-op (no bump, no poll).
* **Disconnect**: one write deletes the account and all its rows (refresh
  token included) — from then on nothing can use it — and records a
  tombstone for the Google `sub` (kept 1 hour, nothing else stored). An OAuth
  callback for that `sub` whose login started before the tombstone, or that
  arrives while revocation is still running, is rejected ("disconnect in
  progress, try again"), so a stale callback cannot resurrect the account and
  the revocation cannot kill a fresh grant. Then a best-effort
  revoke at Google with the token held in memory, 5 s timeout; the reply says
  whether revocation was confirmed. "All your data" means this service's
  stored data; Zulip DMs already sent stay in Zulip.
* **Lost access** (`invalid_grant`): status `disconnected` (guarded by
  `token_rev`), `schedule_rev` bumped, pending reminders deleted, one DM with the reconnect link.
  Signing in again re-activates the same account.

### 3.4 Zulip bot input

* Only new `message` events (never `update_message`: editing a message never
  runs a command). Only one-to-one DMs: the bot plus one human sender. The
  bot's own messages are ignored. Raw content (`apply_markdown=false`).
* Each handled message id is recorded (`handled_messages`, kept 7 days, not
  tied to accounts) in the same write as the command's effect — insert first,
  zero rows inserted means already handled — so a replayed event is never
  executed twice, even after a crash. Revocation and replies happen after
  that write. Messages older than the sender's
  current link are ignored, so a replayed `disconnect` cannot hit a freshly
  reconnected account.
* Commands are the whole trimmed message, case-insensitive, optional leading
  `/`: `stop`, `start`, `disconnect`, `help`. Anything else gets help.
  `start` on a disconnected account replies with the reconnect link.
* **Link codes**: 6 chars, unambiguous alphabet, 15 min TTL, at most one live
  code per account, consumed atomically, all of the account's codes deleted
  on any successful link, only links an unlinked account, wrong attempts
  throttled per sender. Checked before commands.
* The event queue is not durable: a DM sent while the service is down can be
  missed. Every command and code gets a reply; the help text says "no reply
  within a minute → send again or use the website".

## 4. Which events produce a reminder

For each watched calendar (default: primary), `events.list` with
`singleEvents=true`, `timeMin=now`, `timeMax=now+26h`, every page
(`nextPageToken`). The response also carries the calendar's
`defaultReminders`.

**Snapshot is all-or-nothing per account**: if any watched calendar or page
fails, the poll changes nothing and the previous reminders stay. Only a
complete snapshot is reconciled.

An occurrence gets reminders unless:

* it is cancelled; or it has already started and has no reminder row yet
  (an existing `pending` row stays through its delivery grace, §5);
* it is all-day (no reminders for all-day events in this version);
* the user declined it (their `attendees[].self` is `declined`) — setting, on
  by default.

**Timing** — setting with two modes:

* *As in Google Calendar* (default): the event's own reminder offsets
  (`reminders.overrides`, or the calendar's `defaultReminders` when
  `useDefault=true`), any method, duplicates merged, 0–1440 minutes. An event
  explicitly set to no reminders gets none.
* *N minutes before*: one reminder at the user's choice (1–60, default
  `DEFAULT_LEAD_MINUTES`).

**Late discovery and catch-up** (meeting created at short notice, first
connect, resume, restart, Google outage): for each occurrence, offsets whose
fire time is already past collapse into at most **one** immediate reminder,
and only if the meeting starts within 60 minutes; the other overdue offsets
are recorded as `skipped`. So connecting never floods anyone, and a meeting
created 5 minutes ahead still gets its DM.

**Keys**: `account | iCalUID | occurrence start (UTC) | offset`. The same
meeting in two watched calendars is one reminder; when the copies differ, the
payload comes from the primary calendar, else the first calendar in the
user's list.

**Reconcile** (one write, guarded by `schedule_rev`):

* desired key not in the table → insert `pending` (or `skipped`, above);
* existing `pending` row → update its payload (title, place, link may change),
  keep its `fire_at` — a row waiting for a Zulip retry keeps its due time;
* `pending` (or `sending`) row not desired any more → delete;
* rows in any other state are history and never change.

## 5. Delivery

Reminder states: `pending → sending → sent`, or `pending → expired`, or
`skipped`. Sender every 30 s:

* claims due `pending` rows (§3.1), renders, sends, stores `sent` + the Zulip
  message id;
* a row is due at `fire_at`; it is deliverable until 2 minutes after the
  meeting start (so 0-minute reminders work: "starting now"), then `expired`;
* temporary Zulip errors → back to `pending`, retried next tick until
  expiry; a permanent recipient error (deactivated user) → account unlinked,
  its pending rows deleted.

Welcome, lost-access and test messages are sent directly by the action that
causes them, not through the reminders table.

```
📅 **Weekly sync** starts <time:2026-10-06T14:00:00Z> (in 10 min)
📍 Room 4 · 🎥 [Join Google Meet](https://meet.google.com/...) · [Open in Calendar](https://calendar.google.com/...)
```

Zulip renders `<time:…>` in each reader's own timezone. "in N min" is computed
at send time. Video link from `conferenceData` / `hangoutLink`, or the first
`https://` URL in `location`. Titles and locations are escaped for Zulip
markdown. The website shows times with the browser's locale
(`Intl.DateTimeFormat`), not server-side.

## 6. Endpoints

| endpoint | who | auth |
|---|---|---|
| `GET /` | anyone | none; landing page, or status if signed in |
| `GET /login`, `GET /oauth/callback` | users | OAuth `state` + PKCE |
| `GET /link/status` | users | session (own account only) |
| `GET/POST /settings`, `POST /disconnect`, `POST /logout`, `POST /test-reminder` | users | session + CSRF |
| `GET /healthz` | Docker/proxy | none |

## 7. Not now

* Several Google accounts per Zulip user.
* All-day event reminders (need a local fire time and timezone/DST rules).
* Google reminder offsets over 24 h.
* Google push notifications (`events.watch`) and incremental sync
  (`syncToken`, which cannot be combined with a time window) — polling at
  3 min costs ~480 requests/day per watched calendar; revisit when the
  project's Calendar API quota says so.
* Several Zulip realms per deployment — one deployment per realm.
* Reminders to streams/topics instead of DMs.
* Running more than one instance.
