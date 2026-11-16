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
  index, so a lost disk comes back by itself once the config and the secrets are set again;
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
| `src` | `client` or `server`; `import` for records brought in with `clowctl analytics import` |
| `dm` | dimensions: at most 4 short strings (≤ 64 characters, numbers written as strings) plus `suspect`; rollups are cut by them |
| `p` | other properties: numbers, booleans, strings ≤ 200; only in the raw window and the archive |
| `set`, `once` | person properties to set, or set if not yet set |
| `g` | groups, `{type: key}` |
| `ik` | install key, only on `$first_open` (32 hex characters) |

Optional fields are left out rather than written empty. The record never carries an IP address,
email, name or message text.

`clowctl analytics check <file>` validates a saved stdout (or an import file, plain or `.zst`)
offline and runs it through the same indexer and rollups as the server, on a temporary state.

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
| `account_deleted` | **erases the person**: properties, device links and devices are dropped; only the properties the exclusion rule reads stay, so a deleted test account remains excluded from rollups that are recomputed later. Later records of the user (an offline queue) still count but do not bring the person or its links back. Raw records in the window and the archive are not rewritten: they carry ids, never personal data |

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

**A lost disk.** When a project with an archive has an empty index (a new disk, a new server),
clowdrd restores it within a minute of `clowctl apply` and the secrets: the archived rollups the
disk lacks, the latest snapshot (actors and groups keep the numbers those rollups refer to, and
the catalog comes back too) and the raw days since that snapshot, applied on top. Rollup queries
then answer as before. Without a snapshot it indexes every archived raw day from scratch instead.

**The endpoint must be public.** A project token sets the archive endpoint, and clowdrd connects
to it from inside the server, so an endpoint that resolves to a loopback, private, link-local
(the cloud metadata service) or similar address is refused when dialled, whatever name it goes
by. An S3 on the same host or network is allowed by the server's admin in platform.yml.

The server's side lives in platform.yml (an older clowdrd refuses these keys, so update clowdrd
before adding them):

```yaml
logs:
  analytics: { retention: 35d, max: 2GB }   # the hot window of raw records
analytics:
  archive:
    allow_private: [127.0.0.1:9000]         # archive endpoints (host:port as in the URL) allowed on private addresses
  rollup_cache: 256m                        # decoded rollups kept in memory for queries
```

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
clowctl analytics paths --from main --depth 4 --limit 50
clowctl analytics trends purchase_completed --math p95 --prop price_micros --where 'store=apple|play' --where 'p.price_micros>=1000000'
clowctl analytics trends episode_viewed --by pf --by episode_number      # two keys: "ios / 3"
clowctl analytics installs --since 30d --by campaign            # with D1/D7/D30
clowctl analytics person 870
clowctl analytics events --user 870 --since 7d
clowctl analytics catalog                                        # every event, its dimensions and properties
clowctl analytics status                                         # records, window, rollups, archive, digest
clowctl analytics digest --dry-run                               # yesterday's digest, not sent
```

Every query takes `--since`, `--until`, `--include-test` and `-o json`. Keys are dimensions (`pf`,
`cc`, `dm` keys), `p.<prop>`, `person.<prop>` or `first_source` / `first_campaign` / `first_pf` /
`first_app` / `first_cc`.

- `--by k` splits trends, funnels and retention (retention by `first_*` only); `--by` twice splits by
  two keys, written `a / b`. `installs --by` takes one install dimension.
- `--where` (trends, funnel, paths, events; repeatable, all must hold): `k=v`, `k!=v`, `k>n`, `k>=n`,
  `k<n`, `k<=n`; `k=a|b` (or `k=a,b`) is one of, `k!=a|b` none of, and the same key with `=` twice is
  one of both. A funnel applies it to every step; a step's own conditions follow the event name,
  joined by commas, with lists written `a|b`. Rollup kinds (retention, stickiness, lifecycle,
  installs) refuse it rather than ignore it.
- `--math`: `total`, `uniques`, `dau`, `wau`, `mau`, `per_user_avg`, and over `--prop`: `sum`,
  `avg`, `p50`, `p75`, `p90`, `p95`, `p99` (nearest rank).
- The last row of a trend (`total` in JSON) is the whole range, never the last bucket: the sum of
  the buckets for `total` and `sum`; the distinct actors of the range for `uniques`, so it is at
  most the sum of the buckets; all events over those actors for `per_user_avg`; every value of the
  range for `avg` and the percentiles. `dau`, `wau` and `mau` do not add up over a range, so theirs
  is the average of the daily value (daily actives, or the 7- and 30-day actives as of each day)
  over the days of the range up to today: the row reads `AVG`, and JSON adds
  `"total_kind": "average"`.
- `paths`: `--from` / `--to` a screen, `--depth` 3 to 5 (default 3), `--limit` (default 20, at most
  1000); the top screens come as a table of their own (`screens` in JSON).

Counts by one dimension come from the rollups; anything else (property filters, comparisons,
lists, two keys, sums and percentiles) reads the raw records, so it covers the hot window only. A
raw query holds at most a million records and runs at most two minutes: narrow it when it says so.
Older raw data is in the archive:

```sh
clowctl analytics archive --since 2026-09-01 --until 2026-09-30 > urls.txt   # presigned, 1 hour
duckdb -c "select e, count(*) from read_ndjson([$(sed "s/.*/'&'/" urls.txt | paste -sd,)]) group by 1"
```

## Importing history

Records a product kept before it counted with clowdr come in with one command, once the archive is
configured:

```sh
clowctl analytics import --dry-run history.ndjson.zst    # the check only, nothing sent
clowctl analytics import history.ndjson.zst -e prod      # admin token
```

The file holds records of this format (NDJSON, or NDJSON+zstd), `src: "import"` by convention. The
CLI checks it as `clowctl analytics check` does and refuses invalid records unless
`--allow-invalid` (the server then leaves them out). The server writes each record into the
archived raw file of its **event** day, merged with what that file holds and deduplicated by `id`,
then rebuilds from the first day it touched — from scratch when that is the project's first day,
which an import of older history usually is. Importing the same file again changes nothing.

Days from the first day of the stream on are left out and listed: clowdr archives them from what it
received, and an imported file would hide that day's own records. A history that overlaps the
stream is cut at the day before it begins.

A source that keeps sending elsewhere for a while (old app builds that still report to the previous
tool) is brought in with `--include-live-days`: records of days the stream covers go into it as
late records received now, under the service `import`, deduplicated by `id` against the window, and
their days' rollups are recomputed; the daily archive takes them with the day they were received
on. Like any late record, one counts only within the late window — its day at most 8 days ago —
so such a tail is imported at least weekly; older days are left out and listed. Records of the
same events the services now write themselves must not be in such a file, or they count twice.

```sh
clowctl analytics import --include-live-days tail.ndjson.zst -e prod
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
| `POST …/analytics/import?live_days=1` | admin token; body: records as NDJSON, or NDJSON+zstd (at most 1 GB); writes them into the archive and rebuilds, streams progress; `live_days=1` takes days of the stream as late records (see "Importing history") |
| `POST …/analytics/rebuild?since=` | admin token; re-indexes from the stream and the archive, streams progress. From the first day the project has, the index is built from scratch; from a later day it is kept (actor numbers in older rollups, persons, links and install keys before that day) and the records are applied on top |

A server with analytics lists `analytics` in `GET /v1/version` features.
