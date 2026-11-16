# Product analytics

clowdr keeps a third stream per service for product events: what people did in the product
(installed the app, watched an episode, bought coins), as opposed to what the server did (logs,
traces). As with the others, the service only writes to **stdout**. There is no SDK endpoint, no
third-party tracker and no extra database to run.

| Stream | What goes there | Read with | Kept (platform.yml `logs:`) |
|---|---|---|---|
| analytics | lines starting with `{"$":"analytics"` | `clowctl analytics …` | `analytics`: 35 days, 2 GB per service |

On top of the raw stream clowdr keeps, per project:

- an **index**: devices linked to persons, person properties, install keys, the first platform,
  country and install source of every actor;
- **daily rollups**: active actors, counts and uniques of every event by its dimensions,
  sessions, installs, groups, as compressed bitmaps — enough for trends, retention, stickiness and
  lifecycle over any period, long after the raw records left the hot window;
- an **archive** in the project's own S3 bucket: raw days, rollups and a weekly snapshot of the
  index, so a lost disk is rebuilt with `clowctl analytics rebuild`;
- a **daily digest** and **install notifications** in the project's Telegram chat.

Test persons (person property `test: true`, configurable) are left out of every number unless a
query asks for them with `--include-test`.

## Records

One JSON object per line, written by the product's server (the clients send their events to the
server, which adds the user from the session and the country, drops anything that looks like
personal data and prints the record):

```json
{"$":"analytics","v":1,"id":"0193…","ts":"2026-11-16T10:00:00.123Z","e":"episode_viewed",
 "u":"870","d":"5d1c…","s":"0193…","app":"mobile","pf":"android","ff":"phone",
 "av":"2.3.1","b":"3600123","os":"15","lc":"ru","cc":"DE","env":"prod","src":"server",
 "dm":{"episode_number":"3"},"p":{"drama_id":42,"completed":true},
 "set":{"role":"author"},"once":{"first_pf":"android"},"g":{"library":"12"}}
```

| Field | Meaning |
|---|---|
| `v` | format version, `1`; other versions are refused |
| `id` | event id (UUIDv7); the key clowdr deduplicates on |
| `ts` | event time (UTC); the time it was received is kept separately |
| `e` | event name, `^[$a-z][a-z0-9_]{1,47}$`; `$` names are the standard ones |
| `u` | user id (string), only from the server's session |
| `d` | anonymous device / browser id |
| `s` | session id |
| `app`, `pf`, `ff` | app (`mobile`, `landing`, `dashboard`, `server`, …), platform (`ios`, `android`, `web`, `server`, …), form factor (`phone`, `tablet`, `desktop`, …) |
| `av`, `b`, `os`, `lc`, `cc`, `env` | app version, build, OS version, 2-letter language, ISO-2 country, environment |
| `src` | `client` or `server` |
| `dm` | dimensions: at most 4 short strings (≤ 64 characters, numbers written as strings) plus `suspect`; rollups are cut by them |
| `p` | other properties: numbers, booleans, strings ≤ 200; only in the raw window and the archive |
| `set`, `once` | person properties to set, or set if not yet set |
| `g` | groups, `{type: key}` |
| `ik` | install key, only on `$first_open` (32 hex characters) |

Optional fields are left out rather than written empty. The record never carries an IP address,
email, name or message text.

`clowctl analytics check <file>` validates a saved stdout offline and runs it through the same
indexer and rollups as the server, on a temporary state.

### Events clowdr reads

| Event | What clowdr does with it |
|---|---|
| `$identify` | links `p.anon` (the device id) to `u`; the first link of a device wins |
| `$set` | sets person properties (`set`, `once`) without counting as activity |
| `$first_open` | an install, a reinstall or (on the web) a first visit, see below |
| `$session_start`, `$session_end` | sessions; `$session_end` carries `p.duration_ms` |
| `$pageview` | screens (`dm.screen_name`) for `paths` |
| `$client_error` | `dm.fatal` `"true"` counts toward the crash-free share of the digest |
| `user_signed_up`, `purchase_completed`, `subscription_*`, `purchase_refunded` | the digest's sign-ups and money lines |
| `account_deleted` | **erases the person**: properties, device links and devices are dropped; only the properties the exclusion rule reads stay, so a deleted test account remains excluded from rollups that are recomputed later |

Passive events do not make an actor active (property updates, or sent by the server or the store without the
person doing anything): `$set`, `$push_received`, `subscription_renewed`, `subscription_expired`,
`subscription_canceled`, `subscription_refunded`, `purchase_refunded`, `account_deleted`.

### Installs and reinstalls

Every `$first_open` with an install key is decided once, by the order clowdr first saw it: the
first `$first_open` of a key is an **install**, later ones are **reinstalls** (`2-я`, `3-я`, …). A
`$first_open` with `p.reinstall_hint: true` is a reinstall even when its key is new: on Apple the
install key lives in the keychain and survives the app; the client sets the hint when it finds the
key without its own marker. A `$first_open` without a key (the web) is a **first visit**. Records
with `dm.suspect: "1"` are counted apart and never notified.

## Late events

Clients keep events offline for up to 7 days. Rollups of today and yesterday are recomputed every
15 minutes and those of the last 8 days every hour, so a late event lands in its own day. A
device that signs in later moves its anonymous history to the person in every rollup recomputed
after the link. A query never reads a missing rollup as zero: a day in the hot window without one
is computed on the spot.

## Configuration

In deploy.yml, at the top level (`clowctl apply` sends it, `clowctl diff` shows a difference):

```yaml
analytics:
  archive:                       # or: archive: off
    endpoint: https://<account>.r2.cloudflarestorage.com
    region: auto
    bucket: shop-analytics
    style: path                  # path (default) | virtual
  digest:
    hour: 7                      # UTC; default 7
    lang: ru                     # ru (default) | en
    name: Shop                   # shown in the messages; default: the project
    key_events: [episode_viewed, user_signed_up]
    access: false                # true: page views from the nginx access log (sites without events)
    off: false
  installs: digest               # digest (default): only in the digest | hourly: a summary per hour | each: a message per install (≤ 20 an hour, the rest in the hourly summary)
  exclude: { test: true }        # person properties that leave a person out; default {test: true}
```

Secrets never go into deploy.yml; clowdrd keeps them per project:

```sh
printf %s "$KEY"    | clowctl analytics secret set s3-key
printf %s "$SECRET" | clowctl analytics secret set s3-secret
openssl rand -hex 8 | clowctl analytics secret set s3-prefix      # a random path prefix in the bucket
printf %s "$BOT"    | clowctl analytics secret set telegram-token
printf %s "$CHAT"   | clowctl analytics secret set telegram-chat
```

The archive lays out objects as `<prefix>/v1/<project>/raw/YYYY/MM/YYYY-MM-DD.ndjson.zst`
(split by hour as `…-HH.ndjson.zst` when a day is over 100 MB), `…/rollup/YYYY/YYYY-MM-DD.roll.zst`
and `…/state/YYYY-MM-DD.ndjson.zst` (Sundays). A raw day goes up after it ends (from 01:30 UTC)
and is retried hourly; the digest warns about days that did not.

The hot window is `logs.analytics` in platform.yml. An older clowdrd refuses a platform.yml that
has it, so update clowdrd before adding it.

## Reading

```sh
clowctl analytics trends episode_viewed --since 14d
clowctl analytics trends any --math dau --by pf           # any = any activity
clowctl analytics trends purchase_completed --math sum --prop price_micros --where store=play
clowctl analytics funnel '$first_open' user_signed_up episode_viewed --window 7d
clowctl analytics funnel '$first_open' 'episode_viewed:episode_number=2' --by first_source
clowctl analytics retention --period week --by first_source
clowctl analytics stickiness
clowctl analytics lifecycle --interval week
clowctl analytics paths --from main
clowctl analytics installs --since 30d --by campaign            # with D1/D7/D30
clowctl analytics person 870
clowctl analytics events --user 870 --since 7d
clowctl analytics catalog                                        # every event, its dimensions and properties
clowctl analytics status                                         # records, window, rollups, archive, digest
clowctl analytics digest --dry-run                               # yesterday's digest, not sent
```

Every query takes `--since`, `--until`, `--by` (most kinds), `--where k=v` / `--where k!=v` (repeatable),
`--include-test` and `-o json`. Keys are dimensions (`pf`, `cc`, `dm` keys), `p.<prop>`,
`person.<prop>` or `first_source` / `first_campaign` / `first_pf` / `first_app` / `first_cc`.
Counts by dimensions come from the rollups; property filters, sums and percentiles read the raw
records, so they cover the hot window only. Older raw data is in the archive:

```sh
clowctl analytics archive --since 2026-09-01 --until 2026-09-30 > urls.txt   # presigned, 6 hours
duckdb -c "select e, count(*) from read_ndjson([$(sed "s/.*/'&'/" urls.txt | paste -sd,)]) group by 1"
```

## API

| Route | |
|---|---|
| `POST /v1/projects/{project}/analytics/query` | a query (`kind`: trends, funnel, retention, stickiness, lifecycle, paths, person, events, catalog, installs) |
| `GET …/analytics/status` | what `clowctl analytics status` prints |
| `GET …/analytics/archive?since=&until=` | presigned URLs of the archived raw days |
| `POST …/analytics/digest?day=&dry_run=1` | the digest text; sent unless `dry_run` |
| `GET`, `PUT`, `DELETE …/analytics/config` | the `analytics:` block |
| `PUT …/analytics/secrets/{name}` | `{"value": …}`; empty removes it |
| `POST …/analytics/rebuild?since=` | admin token; re-indexes from the stream and the archive, streams progress |

A server with analytics lists `analytics` in `GET /v1/version` features.
