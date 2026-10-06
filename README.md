# zulip-gcal-service

Google Calendar meeting reminders in Zulip — for every user, with no scripts.

Zulip's stock Google Calendar integration asks each user to install Python,
download credential files and keep a terminal running. This is a small
self-hosted web service instead: users click **Sign in with Google** once and
get a Zulip DM before each meeting.

* One Go binary + SQLite, shipped as a Docker image; deploy with
  docker-compose / Portainer behind your TLS proxy.
* Read-only calendar access; refresh tokens encrypted at rest.
* Reminder times follow the event's own Google reminders (or a lead time the
  user picks); times render in each reader's timezone.

## Quick start

1. Admin, once (~15 min): create a Google OAuth client and a Zulip bot, then
   run the Docker image — step by step in
   [docs/admin-setup.md](docs/admin-setup.md).
2. Announce it to your users with the ready-to-paste text in
   [docs/user-guide.md](docs/user-guide.md).

Design and internals: [docs/design.md](docs/design.md).

License: MIT.
