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

The same package is on npm as `@veksa/clowdr-node` (0.5.0 and later). Installed under the old name, the
imports below stay as they are:

```json
"dependencies": { "clowdr-node": "npm:@veksa/clowdr-node@^0.5.0" }
```
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
