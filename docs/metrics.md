
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
