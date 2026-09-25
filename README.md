# Status page

[![Deploy on velixir](https://velixir.net/img/deploy-on-velixir.svg)](https://velixir.net/new?template=status-page)

A public status page for your services. Polls a list of HTTP endpoints, shows uptime and
latency, and exposes a JSON API. One Go file, no dependencies, no database.

[Deploy it on velixir](https://velixir.net/new?template=status-page) and you have a status page
on your own domain in about a minute.

![Status page](https://velixir.net/new?template=status-page)

## What it does

- Polls each monitor on its own schedule, starting immediately rather than after one interval
- Keeps the last 60 checks per monitor in memory and draws them as a bar strip
- Uptime percentage and last latency per service
- `GET /api/status` returns the raw check history as JSON
- `GET /healthz` for the platform health check
- Refreshes itself every 30 seconds, with no JavaScript

## Configuring it

Two options. The file is the default; the environment variable wins when it is set.

**`monitors.json` in the repo:**

```json
{
  "title": "Service status",
  "monitors": [
    { "name": "Website", "url": "https://example.com", "expectStatus": 200, "intervalSeconds": 60 },
    { "name": "API", "url": "https://example.com/health", "intervalSeconds": 30 }
  ]
}
```

**A `MONITORS` environment variable** holding the same JSON. This is the one to use on
velixir: set it on the app's Environment tab and you can repoint the whole page from the
dashboard without touching the repo.

| Field | Default | Notes |
| --- | --- | --- |
| `name` | the URL | Shown on the page |
| `url` | required | The endpoint to poll |
| `method` | `GET` | |
| `expectStatus` | `200` | Anything else counts as down |
| `intervalSeconds` | `60` | Floored at 5 |

## Running it locally

```bash
go run .
```

Then open http://localhost:8080.

## Deploying

```bash
velixir deploy
```

## What it deliberately does not do

**History is in memory and resets when the app restarts.** That is the trade for zero
dependencies and a build measured in seconds. A status page is mostly read for "is it up
right now" and "was it up an hour ago", and 60 checks covers that.

If you want history that survives a deploy, bind a managed Postgres on velixir and persist
checks: it is a contained change to `store.record` plus a load on startup, and nothing else
in the file has to move. `DATABASE_URL` is already in the environment once you bind one.

**It does not alert.** No email, no webhooks, no paging. Adding a webhook call in `watch`
when a check flips from up to down is about ten lines, and is the usual next thing people
want.

**Scale to zero and monitoring do not mix.** An app that sleeps when idle is not polling
while it sleeps, so run a status page on a plan that stays warm. On velixir that means any
paid tier rather than the scale-to-zero Free plan.

## Licence

MIT.
