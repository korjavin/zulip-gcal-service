# Admin setup

One person sets this up once, in about 15 minutes. No Go or programming
knowledge is needed: you click through Google Cloud and Zulip, fill in one
settings file and start one container. Your users then only open a web page
and click **Sign in with Google**.

Throughout this guide, replace the example addresses with your own:

* `https://calendar.example.com` — where this service will be reachable
  (its `PUBLIC_URL`);
* `https://chat.example.com` — your Zulip server.

## 1. What you need

* A **Google Workspace** organization, and an account in it that may create
  Google Cloud projects.
* **Zulip Server 10 or newer** (Zulip Cloud is fine). An ordinary member
  account is enough, unless your organization restricted who may create bots.
* A server with **Docker** (plain `docker compose` or Portainer), and an HTTPS
  reverse proxy in front of it (for example Traefik) with a DNS name for the
  service, e.g. `calendar.example.com`.

## 2. Google Cloud: a sign-in client

Open <https://console.cloud.google.com> signed in with your Workspace account.

1. **Create a project.** Project picker at the top → **New project** → name
   it e.g. `Zulip Calendar` → **Create**, then select it. Use a project for
   this service only: when a user disconnects, Google revokes their grant to
   the whole project's client.
2. **Enable the Calendar API.** Menu → **APIs & Services** → **Library** →
   search **Google Calendar API** → **Enable**.
3. **Consent screen.** Menu → **Google Auth Platform** (older consoles:
   **APIs & Services** → **OAuth consent screen**) → **Get started**:
   * App name: `Calendar reminders`; user support e-mail: yours;
   * **Audience: Internal**; contact e-mail: yours → **Create**.

   Why *Internal*: only accounts of your organization can sign in, Google does
   not need to verify the app, and sign-ins do not expire after 7 days (they
   do for *External* apps in *Testing*).
4. **Scopes.** **Data Access** → **Add or remove scopes** → tick `openid`,
   `.../auth/userinfo.email` and `.../auth/calendar.readonly` (search for
   "calendar.readonly") → **Update** → **Save**. Calendar access is read-only.
5. **Client.** **Clients** → **Create client** →
   * Application type: **Web application**; name: `zulip-gcal-service`;
   * **Authorized redirect URIs** → **Add URI**:
     `https://calendar.example.com/oauth/callback` (your `PUBLIC_URL` +
     `/oauth/callback`, exactly) → **Create**.

   Copy the **Client ID** and **Client secret** now (newer consoles show the
   secret only once). They go into `GOOGLE_CLIENT_ID` and
   `GOOGLE_CLIENT_SECRET`.

## 3. Zulip: the Calendar bot

In Zulip, signed in as yourself (no administrator rights needed):

1. Gear menu → **Personal settings** → **Bots** → **Add a new bot**.
2. Bot type: **Generic bot**; name: `Calendar`; optionally upload a calendar
   picture as avatar → **Add**.
3. Under **Active bots**, copy the bot's **e-mail** (e.g.
   `calendar-bot@chat.example.com`) and its **API key**. They go into
   `ZULIP_BOT_EMAIL` and `ZULIP_BOT_API_KEY`.

Users receive reminders as direct messages from *Calendar*. The bot belongs to
your account: if your Zulip account is deactivated, so is the bot.

## 4. Deploy

The image is published as `ghcr.io/korjavin/zulip-gcal-service`:
`latest` follows the main branch, `vX.Y.Z` tags are releases (pin one if you
prefer to update deliberately).

> If pulling the image fails with *denied* or *unauthorized*, the package is
> still private: its owner must switch it to public once (GitHub → package →
> **Package settings** → **Change visibility**), or log the server in with
> `docker login ghcr.io` using a GitHub token that can read it.

### 4.1 Settings

Take [`docker-compose.yml`](../docker-compose.yml) and
[`.env.example`](../.env.example) from this repository. Generate the
application secret once, on any machine with `openssl`:

```bash
openssl rand -base64 32
```

Then fill in the settings (copy `.env.example` to `.env`):

| setting | example | what it is |
|---|---|---|
| `PUBLIC_URL` | `https://calendar.example.com` | the address users open; must match the redirect URI from step 2.5 |
| `GOOGLE_CLIENT_ID` | `1234-abc.apps.googleusercontent.com` | from step 2.5 |
| `GOOGLE_CLIENT_SECRET` | | from step 2.5 |
| `ZULIP_SITE` | `https://chat.example.com` | your Zulip address |
| `ZULIP_BOT_EMAIL` | `calendar-bot@chat.example.com` | from step 3 |
| `ZULIP_BOT_API_KEY` | | from step 3 |
| `APP_SECRET` | output of `openssl rand -base64 32` | encrypts stored Google tokens and signs logins. Keep it safe, never change it casually (see section 6) |
| `ALLOWED_DOMAINS` | `example.com` | optional: comma-separated Workspace domains allowed to sign in. Leave empty to rely on the *Internal* consent screen alone |
| `DEFAULT_LEAD_MINUTES` | `10` | optional: default "N minutes before" choice, 1–60 |
| `POLL_INTERVAL` | `3m` | optional: how often each calendar is checked |
| `DATA_DIR` | `/data` | optional: where the database lives inside the container; keep the default |
| `TRAEFIK_HOST` | `calendar.example.com` | only for the Traefik labels below |

If a required setting is missing or malformed, the container stops and its log
lists every problem by name (never the secret values).

### 4.2 Start it

Run **exactly one** instance. Two copies would send every reminder twice.

**Plain Docker Compose:** put `docker-compose.yml` and `.env` in one
directory and run

```bash
docker compose up -d
docker compose logs -f     # "listening" means it started
```

**Portainer:** **Stacks** → **Add stack** → name `zulip-gcal` → **Web
editor**: paste `docker-compose.yml` and change `env_file: .env` to
`env_file: stack.env`. Under **Environment variables** → **Advanced mode**,
paste your filled-in settings → **Deploy the stack**.

### 4.3 HTTPS

Sign-in only works over HTTPS. Point your proxy at port `8080` of the
container. With Traefik (Docker provider): in `docker-compose.yml` remove the
`ports:` lines, uncomment the `labels:` block, set `TRAEFIK_HOST`, and attach
the service to the network Traefik uses, for example:

```yaml
services:
  zulip-gcal:
    # ...as in docker-compose.yml, plus:
    networks: [traefik]
networks:
  traefik:
    external: true
```

Adjust the entrypoint (`websecure`) and certificate resolver (`letsencrypt`)
names in the labels to the ones your Traefik uses.

## 5. Check that it works

1. Open `https://calendar.example.com` → **Sign in with Google** → allow
   read-only calendar access.
2. The page says *Connected* and *Calendar* sends you a welcome DM in Zulip.
   (If your Google and Zulip e-mails differ, the page shows a code and an
   **Open Zulip** button instead: send the code to the bot.)
3. Open **Settings** → **Send a test reminder**. A DM from *Calendar* arrives
   within seconds.

Then announce it to your users with [the user guide text](user-guide.md).

## 6. Backup, updates and the secret

* **Backup** the `data` volume (the database `zulip-gcal.db` inside it): stop
  the container, copy the volume, start it again. Keep `APP_SECRET` with the
  backup — the stored Google tokens cannot be read without it.
* **Updates:** pull the new image and recreate the container
  (`docker compose pull && docker compose up -d`, or **Pull and redeploy** in
  Portainer). Reminders already scheduled survive restarts.
* **Changing `APP_SECRET`** makes every stored Google token unreadable and logs
  everyone out: all users must sign in with Google again. Only rotate it if it
  leaked.

## 7. Google API quota

Each watched calendar costs about **480 Calendar API requests a day** (one
check every 3 minutes). Most users watch one calendar; 200 users watching one
calendar each is about 100 000 requests a day. For a large organization, check
**APIs & Services** → **Google Calendar API** → **Quotas & System Limits** in
your project, or raise `POLL_INTERVAL` (e.g. `5m`), which makes new meetings
take a little longer to get a reminder.

## What your users can do

Everything is on the web page: sign in, then **Settings** for reminder timing,
which calendars, skipping declined events, pause, test reminder and
disconnect. Without leaving Zulip they can DM the *Calendar* bot one word:

* `stop` (or `pause`) — pause reminders; `start` (or `resume`) — turn them
  back on;
* `disconnect` — delete everything this service stores about them and remove
  its Google access;
* `help` — list the commands.
