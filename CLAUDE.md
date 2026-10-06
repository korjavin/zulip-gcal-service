# zulip-gcal-service

Task tracking: bd (beads), prefix `zgc`.

## Build & Test

```bash
gofmt -l . && go vet ./... && go test -race ./...
docker build -t zulip-gcal-service .
```

Unit tests run offline: no Google, no Zulip. Use `net/http/httptest` for every
HTTP boundary (Google OAuth + Calendar API, Zulip API incl. the event queue).

## Architecture

See [docs/design.md](docs/design.md). One Go binary + SQLite; web (Google
sign-in, settings) + poller + sender + Zulip bot loop in one process.
Rules: never log tokens or event contents; keep UX for non-technical users —
no step may require a terminal, a file download or copying an API key.
