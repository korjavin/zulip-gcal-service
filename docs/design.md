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
* `disconnect` — revoke the Google access and delete everything stored about
  the user, refresh token included. Reconnecting is one click on the page.

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
services.

```
browser ──HTTPS──> [web]  Google sign-in, settings pages
                     │
                  [SQLite]  accounts, encrypted refresh tokens, settings,
                     │      reminders (pending/sent/suppressed/expired), link codes
                     │
 Google Calendar <─[poller]  every POLL_INTERVAL per account: events in the
                     │       next 26h → reconcile reminders
                     │
                  [sender]   every 30s: due reminders → Zulip DM
                     │
 Zulip <──────────[bot]      send DMs; long-poll the bot's event queue for
                             incoming one-to-one DMs (link codes, later commands)
```

* **Accounts** have a random, never-reused id (not an SQLite rowid) and a
  `generation` counter bumped on disconnect/pause/settings change.
* **Per-account serialization**: poll, reconcile and settings changes for one
  account run under that account's lock. A poll result is discarded if the
  generation changed while it was fetching. No DB write transaction is held
  across Google or Zulip HTTP calls.
* **Tokens at rest**: Google refresh tokens AES-256-GCM encrypted with a key
  derived from `APP_SECRET`. Never logged. Access tokens only in memory.
* **Sessions**: signed `HttpOnly; Secure; SameSite=Lax` cookie with account id
  + generation, checked against the DB on every request (disconnect
  invalidates every browser). Settings forms carry a CSRF token.
* **OAuth**: authorization-code flow, `access_type=offline`, `state` + PKCE.
  `/login` starts without forced consent; the callback learns the Google `sub`,
  and if this account has no usable refresh token (new, or the stored one is
  dead), it retries once with `prompt=consent`. A callback that fails (missing
  calendar scope, no refresh token after the retry) never overwrites a working
  stored connection. Granted scopes are checked in the token response.
* **Identity**: accounts are keyed by Google `sub`. The Google e-mail is used
  for Zulip auto-match only when `email_verified` and authoritative (`hd`
  present, or a `gmail.com` address); otherwise the DM-code path.
* **Link codes**: 6 chars from an unambiguous alphabet, 15 min TTL, at most
  one live code per account, consumed atomically, all of the account's codes
  invalidated on any successful link. Accepted only in a one-to-one DM
  (bot + one human sender), trimmed exact match, raw content
  (`apply_markdown=false`). Wrong attempts are throttled per sender. A code
  only links an unlinked account. The event queue is not durable: if the bot
  loop is down, the linking page says so and the user just resends the code.
* **Disconnect** revokes the token at Google and deletes the account's rows.
  `invalid_grant` on refresh → the account is marked disconnected and gets one
  DM with the reconnect link.

## 4. Which events produce a reminder

For each watched calendar (default: primary), `events.list` with
`singleEvents=true`, `timeMin=now`, `timeMax=now+26h`, every page
(`nextPageToken`). The response also carries the calendar's
`defaultReminders`. An occurrence gets reminders unless:

* it is cancelled;
* it is all-day (no reminders for all-day events in this version);
* the user declined it (their `attendees[].self` is `declined`) — setting, on
  by default;
* the user paused reminders.

**Timing** — setting with two modes:

* *As in Google Calendar* (default): the event's own reminder offsets
  (`reminders.overrides`, or the calendar's `defaultReminders` when
  `useDefault=true`), any method, duplicates merged. An event explicitly set
  to no reminders gets none. Offsets over 24 h are ignored (documented).
* *N minutes before*: one reminder at `DEFAULT_LEAD_MINUTES` or the user's
  choice (1–60).

**Late discovery**: a reminder whose fire time has already passed when it is
first computed (meeting created at short notice, service restart, Google
outage) is sent immediately, as long as the meeting has not started. Once the
meeting has started, an unsent reminder becomes `expired`.

**Identity and reconciliation**: the reminder key is
`account | iCalUID | occurrence start (UTC) | offset`, so the same meeting
seen in two watched calendars produces one DM. Each poll builds the set of
keys from every calendar that was fetched completely and successfully, and
reconciles:

* new keys → `pending` rows;
* `pending` rows whose key is gone, and whose source calendars were all
  fetched successfully this poll → deleted (cancelled, moved, filtered);
* a calendar that failed (5xx, quota, a failed page) keeps its previous rows
  untouched;
* rows that are no longer `pending` are never touched.

## 5. Delivery

The sender picks `pending` rows with `fire_at <= now`, re-reads the account
state right before sending (paused/disconnected → `suppressed`), sends the DM,
then stores `sent` with the Zulip message id. A crash or an ambiguous timeout
between send and store can produce a duplicate; a lost reminder is worse.
Temporary Zulip errors are retried on the next tick until the meeting starts
(→ `expired`); a permanent recipient error (deactivated user) → `suppressed`
and the account is unlinked.

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
