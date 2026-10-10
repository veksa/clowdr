# Logs and traces

clowdr keeps two streams per service, both fed by the service's **stdout/stderr**. There is
nothing to expose: no log files, no debug endpoints, no tokens in the service.

| Stream | What goes there | Read with | Kept (platform.yml `logs:`) |
|---|---|---|---|
| log | every ordinary line | `clowctl logs <service>` | `app`: 14 days, 200 MB per service |
| trace | lines starting with `{"$":"trace"` | `clowctl trace …` | `trace`: 7 days, 2 GB per service |

A trace is **one unit of work**: an inbound request, one run of a background job, one socket
frame. Everything the service did for it is a record with the same `traceId`: the request and
the response, every model call with the prompt and the answer, every outgoing HTTP call, every
storage operation, its log lines, the job's outcome, optionally its SQL. When a bug comes in as
words ("the order was never sent"), you open that chain instead of grepping a log.

## Logs

Write plain lines to stdout. One event per line; multi-line stack traces are fine but harder to
grep. Errors should contain `error`/`ERROR`: `clowctl events` counts error lines and alerts on a
spike. Do not write log files inside the container: they are lost on every deploy and duplicate
what clowdr already keeps.

```sh
clowctl logs server --since 2h --grep 'ERROR|timeout'
clowctl logs server -f
clowctl logs server --since 1d --download > server.log
```

## Reading traces

```sh
clowctl trace ls --since 2h --errors              # what failed
clowctl trace ls -q '"orderId":88'                # every trace that touched order 88
clowctl trace ls --source job:invoice             # runs of one job
clowctl trace ls --user 42                        # one user's requests
clowctl trace show <traceId>                      # one trace, top to bottom
clowctl trace show <traceId> --verbose --max-chars 200000   # full prompts
clowctl trace last --user 42                      # "they just reported a bug"
clowctl trace errors --since 30m                  # failed calls and warn/error records
clowctl trace records --type llm --since 1h       # every model call
clowctl trace records --type http -f              # live
```

`-q` matches a trace when **any** of its records contains the text (name, source, data), so put
the ids you will search by (`orderId`, `userId`, `jobId`) into record data. Every command takes
`--service` (default: all services of the project) and `-o json`.

`clowctl trace show` prints:

```
═══ TRACE tmg1x2k0004a7 ═════════════════════════════════════
time    2026-09-30T14:02:11.086Z → 2026-09-30T14:02:13.911Z  (2825ms)
who     user=1 source=job:report.build service=server
totals  records=5 llm=1 sql=0 errors=0 cost=$0.004100
models  example/model-large
IN      report.build
OUT     done
────────────────────────────────────────────────────────────
+0ms      job       report.build
          reportId│ 12
+4ms      http      POST api.example.com/v1/render  [812ms]
          url     │ https://api.example.com/v1/render
          status=200
+2410ms   llm       example/model-large  [1790ms $0.004100]
          in=3120 out=96 http=200
          user    │ …
          ← text  │ {"verdict":"pass"}
+2825ms   job       report.build → done  [2825ms]
```

## Record format

Any language can write traces: print one JSON object per line to stdout. The object **must
start with `{"$":"trace"`** — clowdr routes the line by that prefix without parsing it.

```json
{"$":"trace","seq":12,"boot":"lq3k9a","ts":"2026-09-30T14:02:11.086Z","type":"http","traceId":"tmg1x2k0004a7","userId":42,"source":"http:POST /orders","name":"POST /orders","data":{"dir":"in","path":"/orders","body":{"sku":"A1"}}}
```

| Field | | |
|---|---|---|
| `$` | required, first | always `"trace"` |
| `ts` | required | RFC 3339 with milliseconds |
| `traceId` | required | the unit of work; `"-"` for records outside any (boot, timers) — listed by `records`, never as a trace |
| `type` | required | `http`, `llm`, `job`, `log`, `storage`, `db`, `note`, `socket` (others are shown generically) |
| `name` | required | route, model, job step, logger context |
| `seq`, `boot` | recommended | per-process counter and process id: order records within one millisecond |
| `userId` | | number or string |
| `source` | | what started the trace: `http:GET /orders/12`, `job:invoice.send`, `socket:chat.send` |
| `tags` | | small facts about the whole trace (`{"telegramUserId":"42"}`), shown in the trace header |
| `level` | | `debug`, `info`, `warn`, `error` |
| `ok` | | `false` marks a failed call |
| `durationMs`, `costUsd` | | numbers; costs are summed per trace |
| `data` | | free-form object, rendered by type (below) |

How `clowctl trace` reads `data`:

- `http`: `dir` (`in`/`out`), `path` or `url`, `status`, `query`, `body`, `error`. An inbound
  request is two records: `POST /orders` and `POST /orders ←` (the response, with `status`):
  they are the IN and OUT lines of the trace.
- `llm`: `system`, `messages` (`[{role, content}]`), `response: {text, finishReason, error}`,
  `usage: {prompt_tokens, completion_tokens}`, `params`, `status`.
- `job`: the first job record is IN; a later one with `outcome` is OUT.
- `log`: `message`. `db`: `sql`, `args`, `rows`, `error`.

Limits: clowdr stores lines up to 16 MB; keep records far below (clowdr-node cuts at 1 MB).
Never write secrets: mask keys, tokens, passwords and cookies before printing.

## Node services: clowdr-node

The package is served by every clowdr server, so it moves with your project to any cluster.

```sh
clowctl sdk install          # from the directory with deploy.yml
```

It downloads `clowdr-node-<version>.tgz` from your server into `vendor/clowdr/` (commit it) and
adds `"clowdr-node": "file:…"` to the `package.json` of every service with a `path` that has one.
`clowctl sdk update` moves to the server's newest version. Without clowctl:
`curl -O https://<server>/sdk/node/clowdr-node-<version>.tgz` (the index is `/sdk/node/`).

### Wiring (once per service)

```ts
import axios from 'axios';
import {installTracing, httpTraceMiddleware, setTraceUser} from 'clowdr-node';

// main.ts, before listen()
installTracing({axios: [axios], boot: {environment: 'prod'}});
app.use(httpTraceMiddleware());          // first after the body parsers: it wraps guards and filters too

// auth guard, once the user is known
setTraceUser(user.id);
```

Logger: next to the console output, also write the line into the trace, so it shows inside
`trace show`:

```ts
import {traceLog} from 'clowdr-node';
traceLog('error', 'Orders', message);    // level, context, text
```

Model calls: give the OpenAI-compatible client the traced fetch. With `routeHeader`, a header the
service sets on its calls names what each one is for (`data.route`). Tool executions of the AI SDK
are recorded with `traceTools`.

```ts
import {createLlmFetch, llmFetch, traceTools} from 'clowdr-node';
const openai = createOpenAI({apiKey, baseURL, fetch: llmFetch});
const routed = createOpenAI({apiKey, baseURL, fetch: createLlmFetch({routeHeader: 'x-app-route'})});
// cost: when the bill is not usage.cost (OpenRouter BYOK); undefined = unknown, 0 = free
const billed = createLlmFetch({cost: usage => usage.is_byok ? usage.cost_details?.upstream_inference_cost : usage.cost});
await generateText({model, tools: traceTools(tools), headers: {'x-app-route': 'summary'}, ...});
```

Storage, database, cache:

```ts
import {traceS3, tracePrismaAdapter, tracePrismaQuery, tracePgPool, traceRedis} from 'clowdr-node';
const s3 = traceS3(new S3Client({...}));
const prisma = new PrismaClient({adapter: tracePrismaAdapter(new PrismaPg({...}))});   // channel db
prisma.$on('query', tracePrismaQuery);    // or this, with log: [{emit: 'event', level: 'query'}]
tracePgPool(pool);                        // or a node-postgres Pool / Client
const redis = traceRedis(new Redis({...}));                                         // channel cache
```

A call the fetch patch cannot see (an SDK with its own HTTP stack, a mail provider):

```ts
import {traceCall} from 'clowdr-node';
const res = await traceCall({type: 'mail', name: template, data: {to}}, () => mailer.send(msg), r => ({id: r.id}));
```

A service that already wraps every outgoing call that way passes `installTracing({fetch: false})`,
so fetch calls are not recorded twice.

### Background work

Everything not started by a request needs its own trace, or its records land in no trace:

```ts
import {runInTrace, traceJob} from 'clowdr-node';

await runInTrace({source: `job:${step}`, fresh: true}, async () => {
    traceJob(step, {orderId, attempt});                       // IN
    const outcome = await run();
    traceJob(`${step} → ${outcome}`, {outcome}, {ok: outcome !== 'failed'});   // OUT
});
```

Facts about the whole unit of work go into tags, written on every record of the trace (and found by
`-q`): `runInTrace({source: 'telegram:message', tags: {telegramUserId}}, …)` or, once known,
`addTraceTags({tool: name})`.

`runInTrace` joins the surrounding trace when there is one; `fresh: true` starts a new one, for
work that outlives the request that started it. Sockets: open `runInTrace` per frame, then
`setTraceSource('socket:<message>')` once it is decoded.

### Logic that rides on records

Something that must see every record even when tracing is off (billing a model call's cost to
whoever caused it) subscribes instead of reading the stream:

```ts
import {onTraceRecord} from 'clowdr-node';
onTraceRecord((record, trace) => { if (record.costUsd) bill(record.costUsd, trace?.userId); });
```

### Noisy channels

SQL (`db`) and Redis (`cache`) are off by default (one job is dozens of statements and buries the chain). Turn it on for a
while, then off:

```sh
clowctl env set server CLOWDR_TRACE_CHANNELS=db,cache && clowctl restart server
clowctl env set server CLOWDR_TRACE_CHANNELS= && clowctl restart server
```

Records are written where clowdr runs the service (it sets `CLOWDR_SERVICE` in every container)
and nowhere else: a local `yarn start` or a test run keeps a clean console. `CLOWDR_TRACE=1` writes
them locally too (pipe through `grep -v '^{"$":"trace"'` to hide them again), `CLOWDR_TRACE=0` stops
them in a deployment; a test that reads records calls `setTracing(true)`.

### Secrets

Values are masked by key (`authorization`, `cookie`, `password`, `*token`, `*apiKey`,
`*secret`, …) and by shape (`sk-…`, JWTs, `Bearer …`, Telegram bot tokens, AWS key ids);
outgoing URLs lose their query string. Add your own shapes once at start:

```ts
import {addSecretPattern} from 'clowdr-node';
addSecretPattern({key: /^t$/});              // a field name (here: an HMAC in webhook urls)
addSecretPattern({value: /v1:[A-Za-z0-9+/=]{12,}:[A-Za-z0-9+/=]{12,}/});
```

Content is written on purpose: prompts, answers and request bodies are what a trace is read
for. Access to traces is access to the project (a project token), like its logs.

### Rules

- One unit of work, one trace. Never reuse a trace id across jobs.
- Put the ids you search by into `data`, under stable names.
- Health checks are not recorded while they succeed (`httpTraceMiddleware({quiet})`).
- Every response carries `X-Trace-Id`: ask for it in bug reports, then `clowctl trace show <id>`.
- A 4xx response is `ok` with level `warn` (shown by `trace errors`); `trace ls --errors` lists
  traces with a 5xx, a failed call or an `error` line.
- Do not keep a second copy: no trace files, no debug endpoints. clowdr is the store.

# Metrics

Like logs and traces, metrics leave a service through **stdout**: no `/metrics` endpoint, no
scraper, no hash in a URL. A service writes one point per series per minute; clowdr keeps the
points in the service's metric stream (platform.yml `logs.metric`: 30 days, 500 MB per service)
and aggregates them when you ask.

clowdr adds, for every running container, once a minute: `container_cpu_percent`,
`container_memory_bytes`, `container_memory_limit_bytes`. HTTP rates, statuses and latencies come
from the nginx access log (`clowctl access`), so services do not need HTTP metrics of their own.

## Reading

```sh
clowctl metrics ls                                              # names, kinds, labels, last values
clowctl metrics show orders --since 24h --step 1h --by outcome  # a counter split by a label
clowctl metrics show orders --agg rate --step 5m --since 2h     # per second
clowctl metrics show queue_depth --step 10m --agg max           # a gauge
clowctl metrics show job_seconds --agg p95 --where step=render  # a histogram, filtered
clowctl metrics show container_memory_bytes --by service --step 5m --since 6h
```

Rows are time buckets of `--step` (at least 1m), columns the series of `--by` (any label, plus
`service` and `replica`). Aggregation, `--agg`:

| Kind | Default | Others |
|---|---|---|
| counter | `sum` of increases in the bucket | `rate` (per second) |
| gauge | `max` over replicas of each one's last value in the bucket | `last`, `min`, `avg`, `sum` |
| histogram | `p95` | `p50`, `p90`, `p99`, `avg`, `count`, `max`, `sum` |

A gauge is `max` across replicas by default because several workers usually report the same
state (a queue depth read from the database); use `sum` for per-worker values (connections).

## Point format

Any language can write metrics: one JSON object per line on stdout, starting with
`{"$":"metric"`, once a minute per series.

```json
{"$":"metric","ts":"2026-09-30T14:02:00.000Z","name":"orders","kind":"counter","value":3,"labels":{"outcome":"paid"}}
{"$":"metric","ts":"2026-09-30T14:02:00.000Z","name":"queue_depth","kind":"gauge","value":12}
{"$":"metric","ts":"2026-09-30T14:02:00.000Z","name":"job_seconds","kind":"histogram","labels":{"step":"render"},"count":5,"sum":12.3,"min":0.4,"max":6.1,"bounds":[1,2.5,5,10],"counts":[1,2,1,1,0]}
```

- **counter**: `value` is the increase since the last point (not a running total). Skip a
  minute with no increase.
- **gauge**: `value` is the current value.
- **histogram**: `bounds` are bucket upper bounds, `counts` has one more entry (above the last
  bound); `count`, `sum`, `min`, `max` cover the minute. Keep the same `bounds` for a name, or
  quantiles across points cannot be computed.
- `labels`: a few low-cardinality values (an outcome, a step, a store). Never a user id or a URL:
  every distinct label set is a series.

## Node services: clowdr-node

```ts
import {metrics} from 'clowdr-node';

metrics.counter('orders', {outcome: 'paid'});            // +1 (or metrics.counter(name, labels, n))
metrics.gauge('ws_connections', sockets.size);            // set; written every minute until changed
metrics.observe('job_seconds', seconds, {step});          // histogram, default buckets in seconds
await metrics.time('job_seconds', {step}, () => run());   // the same, with outcome=ok|error
metrics.buckets('payload_bytes', [1e3, 1e4, 1e5, 1e6]);   // other buckets, before the first observe

// State the process does not see as events, read at every flush (async is fine):
metrics.gaugeFn('queue_depth', () => db.task.count({where: {status: 'queued'}}));
```

Points are written only where clowdr runs the service (or with `CLOWDR_TRACE=1`), like traces.
Each worker of a Node cluster writes its own points; clowdr adds them up, so no aggregation
between workers is needed. Tests read points with `setMetricsWriter(fn)` and `flushMetrics()`.

### Metrics you get from tracing

`installTracing()` turns trace records into metrics (`installTracing({metrics: false})` turns it off):

| Metric | Labels | From |
|---|---|---|
| `llm_calls`, `llm_cost_usd`, `llm_tokens`, `llm_seconds` | model, route, outcome / direction | every model call (`llmFetch`, `createLlmFetch`) |
| `tool_calls`, `tool_seconds` | tool, outcome | `traceTools` |
| `outbound_requests`, `outbound_seconds` | host, status (2xx…5xx, error) | outgoing fetch and axios |
| `calls`, `call_seconds` | type, outcome | any other timed call: `traceCall`, `traceMethods`, `traceS3`, Telegram… |
| `log_lines` | level (warn, error), context | `traceLog` |

```sh
clowctl metrics show llm_cost_usd --by route --since 7d --step 1d
clowctl metrics show outbound_requests --by host,status --since 24h
clowctl metrics show log_lines --by context --where level=error --since 24h
```

### Probes

```ts
metrics.probe('db', () => prisma.$queryRaw`SELECT 1`);   // db_probe_seconds, db_probe_failures, every 5 s
```

Return `false` from the function to skip a round (not connected yet).

## CPU profiles

```sh
clowctl profile server --seconds 30          # every running replica, every cluster worker
clowctl profile server --replica 2 -O /tmp
```

Each process that called `installTracing()` (clowdr-node 0.4+, under clowdr) profiles itself with
Node's built-in inspector and clowctl downloads `<service>-r<replica>-<pid>-<time>.cpuprofile`:
open it in Chrome DevTools → Performance → Load profile. There is no endpoint: the request goes
through `clowctl exec` and SIGUSR2 to the processes that registered in `/tmp/clowdr-node`.
`installTracing({profile: false})` opts out.
