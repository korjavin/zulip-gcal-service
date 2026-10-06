# Changelog

## v0.1.0

First release. Requires Zulip Server 10+ (feature level 302+) and a Google
Workspace organization.

* **Sign in with Google**: OAuth with PKCE, read-only calendar scope,
  optional `ALLOWED_DOMAINS`; refresh tokens encrypted at rest.
* **Zulip linking**: automatic match by Google e-mail, or a 6-character code
  sent to the Calendar bot as a DM.
* **Reminders as Zulip DMs** for timed events in the next 26 hours: timing
  as in Google Calendar or N minutes before, catch-up for meetings created at
  short notice, the same meeting in several calendars reminded once, times
  shown in each reader's timezone, video and Calendar links.
* **Pages**: landing, status (next reminder, paused, lost access, stale
  sync), settings (timing, calendars, skip declined events, pause, test
  reminder, disconnect), friendly error pages.
* **Bot DM commands**: `stop` / `start`, `disconnect`, `status` (connection,
  timing, next reminders), `today` (meetings still ahead today, read live
  from Google Calendar in the Zulip profile timezone), `help`.
* **Lost Google access**: one DM with a reconnect link; signing in again
  resumes the same account.
* **Disconnect** deletes everything the service stores about the user and
  revokes the Google access.
* **Deployment**: one Go binary + SQLite, multi-arch Docker image on GHCR,
  `docker-compose.yml` with optional Traefik labels, built-in health check;
  [admin setup guide](docs/admin-setup.md) and
  [user announcement text](docs/user-guide.md).
