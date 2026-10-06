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

Status: in design. See [docs/design.md](docs/design.md).

License: MIT.
