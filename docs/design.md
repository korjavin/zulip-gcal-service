# zulip-gcal-service: design

Google Calendar reminders in Zulip, for everyone in the organization, without
anybody running a script. One self-hosted HTTPS service, one Zulip bot, one
Google OAuth client. A user clicks **Sign in with Google** once and from then on
gets a Zulip DM before each meeting.

Replaces Zulip's stock Google Calendar integration, which asks every user to
install Python, download `client_secret.json` and a `zuliprc`, and keep a
terminal (or `screen`) open forever.

## 1. What a user does

1. Opens `https://calendar.example.com` and clicks **Sign in with Google**.
   Google asks for read-only access to their calendar.
2. Done. The page says "Connected — reminders go to you in Zulip as DMs from
   *Calendar*", and the bot sends a welcome DM.

If the service cannot find the user's Zulip account by their Google e-mail
(different addresses, or e-mail hidden by the realm's visibility settings), the
page shows a short code and a button **Open Zulip**, which opens a DM with the
bot. The user sends the code; the bot links that Zulip account and replies.
The Zulip side identifies the sender, so no e-mail lookup is needed.

Everything else is optional, on the settings page (same Google sign-in):
reminder lead time, which calendars, skip declined/all-day events, pause,
disconnect.

## 2. What an admin does (once)

1. Google Cloud: create an OAuth client (Web application), redirect URI
   `https://calendar.example.com/oauth/callback`, scopes `openid email
   calendar.readonly`. For a Google Workspace org make the consent screen
   **Internal**: no Google verification, refresh tokens do not expire, only
   the org's accounts can sign in. (An *External* app in *Testing* status
   loses refresh tokens after 7 days; *External/Production* needs Google's
   verification for the sensitive calendar scope.)
2. Zulip: create a **Generic bot** named "Calendar", copy its e-mail and API
   key.
3. Deploy `docker-compose.yml` (Portainer stack or plain compose) with the
   env below, behind any TLS reverse proxy (Traefik labels included).

| env | required | meaning |
|---|---|---|
| `PUBLIC_URL` | yes | `https://calendar.example.com`, used for redirect URI and links |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | yes | OAuth client |
| `ZULIP_SITE`, `ZULIP_BOT_EMAIL`, `ZULIP_BOT_API_KEY` | yes | the Calendar bot |
| `APP_SECRET` | yes | 32+ random bytes, base64; subkeys for token encryption and cookies are derived from it (HKDF) |
| `ALLOWED_EMAIL_DOMAINS` | no | comma-separated; refuse other Google accounts (on top of an Internal consent screen) |
| `DATA_DIR` | no | default `/data` (SQLite file lives here; mount a volume) |
| `DEFAULT_LEAD_MINUTES` | no | default 10 |
| `POLL_INTERVAL` | no | default `3m` |

## 3. Architecture

One Go binary, one SQLite file, no other services.

```
browser ──HTTPS──> [web]  Google sign-in, settings pages
                     │
                  [SQLite]  users, encrypted refresh tokens, settings,
                     │      upcoming reminders, sent-log (dedupe)
                     │
 Google Calendar <─[poller]  every POLL_INTERVAL per user: events in the
                     │       next 26h → (re)compute reminders
                     │
                  [sender]   every 30s: due reminders → Zulip DM, mark sent
                     │
 Zulip <──────────[bot]      send DMs; long-poll the bot's event queue for
                             incoming DMs (link codes, later: commands)
```

* **Tokens at rest**: Google refresh tokens AES-256-GCM encrypted with a key
  derived from `APP_SECRET`. Never logged. Access tokens only in memory.
* **Sessions**: signed, `HttpOnly; Secure; SameSite=Lax` cookie with the user
  id; settings forms carry a CSRF token.
* **OAuth**: authorization-code flow, `access_type=offline`, `state` + PKCE,
  `prompt=consent` only when no refresh token is stored yet.
* **Disconnect** revokes the token at Google and deletes the user's rows.
  `invalid_grant` on refresh (user removed access in their Google account) →
  the user is marked disconnected and gets one DM with the reconnect link.

## 4. Which events produce a reminder

For each watched calendar (default: primary), `events.list` with
`singleEvents=true`, `timeMin=now`, `timeMax=now+26h`. An occurrence gets a
reminder unless:

* it is cancelled;
* the user declined it (their `attendees[].self` response is `declined`) —
  setting, on by default;
* it is all-day — setting, on by default;
* the user paused reminders.

Fire time: the event's own popup reminders if it has any (`reminders.overrides`
with method `popup`, or the calendar's `defaultReminders` when
`useDefault=true`), else the user's lead time. Setting: "use Google's reminder
times" (default on) vs. "always N minutes before".

Dedupe key: `user | calendarId | eventId | start (RFC 3339 UTC) | fire offset`.
Each poll recomputes the user's future reminders; a moved event simply gets a
new key, a cancelled one loses its unsent reminder. A sent key is never sent
again.

## 5. The message

```
📅 **Weekly sync** starts <time:2026-10-06T14:00:00Z> (in 10 min)
📍 Room 4 · 🎥 [Join Google Meet](https://meet.google.com/...) · [Open in Calendar](https://calendar.google.com/...)
```

Zulip renders `<time:…>` in each reader's own timezone, so the service needs no
timezone handling for reminders. Video link from `conferenceData` /
`hangoutLink`, or the first `https://` URL in `location`.

## 6. Endpoints

| endpoint | who | auth |
|---|---|---|
| `GET /` | anyone | none; landing page or "connected" status if signed in |
| `GET /login`, `GET /oauth/callback` | users | OAuth `state` + PKCE |
| `GET/POST /settings`, `POST /disconnect`, `POST /logout` | users | session cookie + CSRF |
| `GET /healthz` | Docker/proxy | none |

## 7. Not now

* Google push notifications (`events.watch`) instead of polling — needs a
  public webhook and channel renewal; polling at 3 min is enough.
* Incremental sync with `syncToken` — add when API quota matters.
* Several Zulip realms per deployment — one deployment per realm.
* Reminders to streams/topics instead of DMs.
