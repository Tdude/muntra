# Muntra

Lightweight, GDPR-friendly web analytics. **Single Go binary, ~22 MB image, ~50 MB RSS regardless of traffic.** Built as a drop-in replacement for Umami without the Next.js + Prisma memory creep.

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)

[Article here](https://idunworks.com/blog/building-muntra).

## Why

Umami is arguably a great product but it's a Next.js app that drifts to 1 GB RAM over a few days. Most people solve it by restarting it. That is an architectural decision I can't live with. If you have a small VPS hosting a handful of sites, that's most of your memory budget for analytics that should be a rounding error. I have enough challenges with my own memory. Don't need the same on my servers!

Muntra is:

- **One static Go binary**, distroless image, ~22 MB total.
- **Bounded memory** — Redis as buffer (128 MB hard cap), Postgres as durable store, no in-process caches that grow.
- **GDPR-clean by design** — visitor identifier is `sha256(ip + ua + daily_salt)`. The salt is regenerated each UTC midnight, kept in Redis with a 25-hour TTL, then deleted. After deletion, no party (including you) can correlate hashes across days.
- **Bot filtering at ingest** — rejects known bot user-agents before they touch the database.
- **Origin validation** — per-site Origin/Referer allowlist on `/collect` stops anyone from spoofing events.
- **Schema migrations auto-apply** on container startup. Adding columns to `raw_events` is a `.sql` file in `schema/`, idempotent (`ADD COLUMN IF NOT EXISTS`).
- **First-party serveable** — front Muntra under `your-domain.com/muntra/` via nginx and ad-blockers stop blocking it.

What it tracks: pageviews, unique visitors, sessions, top URLs, referrers, browsers (with versions), OS (with versions), device class, country (if you wire MaxMind), viewport, timezone, screen, pixel ratio, custom events. Daily rollups maintained automatically.

## Quickstart

```sh
git clone https://github.com/Tdude/muntra.git
cd muntra
cp .env.example .env
$EDITOR .env                          # set MUNTRA_DB_PASSWORD + MUNTRA_DASHBOARD_TOKEN
docker compose up -d
curl http://127.0.0.1:8090/health      # {"status":"ok"}
```

That's it — the migrations run on first start, the salt rotates itself, the flush worker writes batches to Postgres every 60s.

## Integrate the tracker

Drop one `<script>` tag into your page (any framework, any backend). The tracker is ~1 KB minified, async, no dependencies.

```html
<script async
        src="https://your-domain.com/muntra/script.js"
        data-site-id="mysite"></script>
```

The site ID must be in `MUNTRA_ALLOWED_SITES`. The script auto-detects the `/collect` endpoint from its own URL — so `https://your-domain.com/muntra/script.js` will POST to `https://your-domain.com/muntra/collect`. No CORS issues if served first-party.

For SPA navigation (Svelte / React / Vue / Next.js), the tracker patches `history.pushState`/`replaceState` and re-fires a pageview on each route change. Nothing to wire up.

To track custom events:

```js
window.muntra.track('signup', { plan: 'pro' });
```

## Configuration

| Env var                  | Default                     | Purpose                                                                 |
| ------------------------ | --------------------------- | ----------------------------------------------------------------------- |
| `MUNTRA_HTTP_ADDR`        | `:8090`                     | Listen address                                                          |
| `MUNTRA_REDIS_URL`        | `redis://localhost:6379/0`  | Event buffer + salt store                                               |
| `MUNTRA_POSTGRES_DSN`     | _required_                  | Durable store                                                           |
| `MUNTRA_ALLOWED_SITES`    | _required_                  | Comma-separated site IDs accepted by `/collect`                         |
| `MUNTRA_SITE_ORIGINS`     | _empty_                     | Per-site Origin allowlist. `site:host1,host2\|site:host3` (see below)   |
| `MUNTRA_DASHBOARD_TOKEN`  | _required_                  | Bearer secret for `/api/*` dashboard queries                            |
| `MUNTRA_FLUSH_INTERVAL`   | `60s`                       | How often the flush worker drains Redis → Postgres                      |
| `MUNTRA_FLUSH_BATCH_SIZE` | `500`                       | Max events per `COPY` batch                                             |
| `MUNTRA_ROLLUP_INTERVAL`  | `15m`                       | How often pre-aggregated tables get refreshed                           |

### Site Origins (anti-spoof, recommended)

By default `/collect` accepts any Origin if the `site_id` is allowed. Real production sites should constrain this. Example:

```sh
MUNTRA_SITE_ORIGINS=mysite:example.com,www.example.com|blog:blog.example.com
```

Now `/collect` POSTs with `site_id=mysite` only succeed if the request's Origin (or Referer fallback) host is `example.com` or `www.example.com`. Anything else → `403`.

## Endpoints

Public (CORS-friendly, no auth — meant for browser):

- `POST /collect` — ingest one event. Tracker JS posts here.
- `GET  /script.js` — serves the embedded tracker.
- `GET  /health` — `{"status":"ok"}`.

Dashboard API (require `Authorization: Bearer $MUNTRA_DASHBOARD_TOKEN`):

- `GET /api/stats?site=X&from=ms&to=ms` — totals + previous-period deltas (pageviews, visitors, visits).
- `GET /api/timeseries?site=X&from=ms&to=ms&unit=hour|day|month` — bucketed points.
- `GET /api/breakdown?site=X&by=url|referrer|country|browser|os|device&limit=N` — top-N grouping.
- `GET /api/live?site=X` — active visitors in the last 5 min.

Time ranges are unix milliseconds. Browse the response shapes in `internal/api/handler.go`.

## Front Muntra behind nginx (first-party)

See [`docs/nginx.example.conf`](docs/nginx.example.conf) for a complete vhost. Key block:

```nginx
location /muntra/ {
    proxy_pass http://127.0.0.1:8090/;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

The trailing slash on `proxy_pass` strips the `/muntra/` prefix so the container sees `/script.js`, `/collect`, `/api/*`.

Serving first-party means ad-blockers don't recognize the path as a tracker — events from real users actually arrive.

## Schema

Single durable table — `raw_events` — plus a rollup table per analytic dimension. See `schema/0001_init.sql`.

The migration runner (`internal/migrate`) walks `schema/*.sql` in lexical order on startup and runs each via `pgx.Exec`. Every file must be idempotent (`CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`). No version-tracking table — we just re-run everything every boot. Cheap, removes "did the migration apply?" footguns.

Adding fields = drop a new SQL file in `schema/`, restart the container.

## What it doesn't do (yet)

- MaxMind GeoLite2 lookup — country column stays empty unless you wire it. PRs welcome.
- Multi-region deployments — single Redis + single Postgres.
- A polished web admin — Muntra is the *ingestion + API* layer; bring your own dashboard. The `/api/*` endpoints are stable.
- E-commerce conversion tracking — events are pageviews + arbitrary custom events; you compose funnels client-side.

## License

[AGPL-3.0-or-later](LICENSE). If you run a modified version as a network service, you must publish the modifications under the same license. The unmodified version can be self-hosted freely.
