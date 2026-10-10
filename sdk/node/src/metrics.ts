// Metrics: kept in the process and written to stdout once a minute, one line per series —
// a counter's increase over the minute, a gauge's value, a histogram's bucket counts. clowdr keeps
// them in the service's metric stream; read them with `clowctl metrics`. See docs/metrics.md.

import {isTracing} from './record';

export type Labels = Record<string, string | number | boolean | undefined>;

/** Default histogram buckets, in seconds: request and job durations. */
export const DEFAULT_BOUNDS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300];

type GaugeFn = () => number | undefined | Promise<number | undefined>;

interface CounterSeries {kind: 'counter'; name: string; labels: Record<string, string>; value: number}
interface GaugeSeries {kind: 'gauge'; name: string; labels: Record<string, string>; value: number}
interface HistogramSeries {
    kind: 'histogram'; name: string; labels: Record<string, string>;
    bounds: number[]; counts: number[]; count: number; sum: number; min?: number; max?: number;
}

const series = new Map<string, CounterSeries | GaugeSeries | HistogramSeries>();
const gaugeFns = new Map<string, {name: string; labels: Record<string, string>; fn: GaugeFn}>();
const bounds = new Map<string, number[]>();

let write: (line: string) => void = line => {
    process.stdout.write(line);
};
let timer: ReturnType<typeof setInterval> | null = null;

function clean(labels?: Labels): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(labels ?? {})) {
        if (v !== undefined) out[k] = String(v);
    }
    return out;
}

function key(name: string, labels: Record<string, string>): string {
    return name + JSON.stringify(Object.keys(labels).sort().map(k => [k, labels[k]]));
}

function start(): void {
    if (timer || !isTracing()) return;
    timer = setInterval(() => void flushMetrics(), 60_000);
    timer.unref?.();
    process.once('beforeExit', () => void flushMetrics());
}

export const metrics = {
    /** Count something: `metrics.counter('orders', {outcome: 'paid'})`. */
    counter(name: string, labels?: Labels, n = 1): void {
        try {
            const l = clean(labels);
            const k = key(name, l);
            const s = series.get(k);
            if (s && s.kind === 'counter') s.value += n;
            else series.set(k, {kind: 'counter', name, labels: l, value: n});
            start();
        } catch {
            // metrics are best-effort, like traces
        }
    },

    /** Set a value that goes up and down: `metrics.gauge('ws_connections', n)`. Written every minute until changed. */
    gauge(name: string, value: number, labels?: Labels): void {
        try {
            const l = clean(labels);
            series.set(key(name, l), {kind: 'gauge', name, labels: l, value});
            start();
        } catch {
            // best-effort
        }
    },

    /**
     * A value read at every flush, for state the process does not track as events: a queue depth
     * from the database, a pool size. Async is fine; a throwing or undefined result writes nothing.
     */
    gaugeFn(name: string, fn: GaugeFn, labels?: Labels): void {
        const l = clean(labels);
        gaugeFns.set(key(name, l), {name, labels: l, fn});
        start();
    },

    /** Record a duration or a size into a histogram: `metrics.observe('job_seconds', 1.7, {step})`. */
    observe(name: string, value: number, labels?: Labels): void {
        try {
            if (!Number.isFinite(value)) return;
            const l = clean(labels);
            const k = key(name, l);
            let s = series.get(k);
            if (!s || s.kind !== 'histogram') {
                const b = bounds.get(name) ?? DEFAULT_BOUNDS;
                s = {kind: 'histogram', name, labels: l, bounds: b, counts: new Array(b.length + 1).fill(0), count: 0, sum: 0};
                series.set(k, s);
            }
            let i = 0;
            while (i < s.bounds.length && value > s.bounds[i]) i++;
            s.counts[i]++;
            s.count++;
            s.sum += value;
            s.min = s.min === undefined ? value : Math.min(s.min, value);
            s.max = s.max === undefined ? value : Math.max(s.max, value);
            start();
        } catch {
            // best-effort
        }
    },

    /** Time an async function into a histogram, in seconds, labelled with its outcome. */
    async time<T>(name: string, labels: Labels, fn: () => Promise<T>): Promise<T> {
        const started = Date.now();
        try {
            const r = await fn();
            metrics.observe(name, (Date.now() - started) / 1000, {...labels, outcome: 'ok'});
            return r;
        } catch (e) {
            metrics.observe(name, (Date.now() - started) / 1000, {...labels, outcome: 'error'});
            throw e;
        }
    },

    /** Buckets for a histogram, before its first observation (default: DEFAULT_BOUNDS, seconds). */
    buckets(name: string, upperBounds: number[]): void {
        bounds.set(name, [...upperBounds].sort((a, b) => a - b));
    },
};

/** Write every series now (called once a minute, and before exit). */
export async function flushMetrics(now: Date = new Date()): Promise<void> {
    if (!isTracing()) return;
    const ts = now.toISOString();
    const out = (p: Record<string, any>) => {
        try {
            write(JSON.stringify({$: 'metric', ts, ...p}) + '\n');
        } catch {
            // best-effort
        }
    };
    for (const [k, s] of series) {
        if (s.kind === 'counter') {
            if (s.value !== 0) out({name: s.name, kind: 'counter', value: s.value, labels: nonEmpty(s.labels)});
            series.delete(k); // a counter writes its increase: the next minute starts at zero
        } else if (s.kind === 'gauge') {
            out({name: s.name, kind: 'gauge', value: s.value, labels: nonEmpty(s.labels)});
        } else if (s.count > 0) {
            out({name: s.name, kind: 'histogram', labels: nonEmpty(s.labels), count: s.count, sum: s.sum, min: s.min, max: s.max, bounds: s.bounds, counts: s.counts});
            series.delete(k);
        }
    }
    await Promise.all([...gaugeFns.values()].map(async g => {
        try {
            const v = await Promise.race([g.fn(), new Promise<undefined>(r => setTimeout(() => r(undefined), 5_000).unref?.())]);
            if (typeof v === 'number' && Number.isFinite(v)) out({name: g.name, kind: 'gauge', value: v, labels: nonEmpty(g.labels)});
        } catch {
            // a failing probe writes nothing for this minute
        }
    }));
}

function nonEmpty(l: Record<string, string>): Record<string, string> | undefined {
    return Object.keys(l).length ? l : undefined;
}

/** Where metric lines go, and a clean slate; for tests. */
export function setMetricsWriter(fn: ((line: string) => void) | null): void {
    write = fn ?? (line => {
        process.stdout.write(line);
    });
    series.clear();
    gaugeFns.clear();
    bounds.clear();
    if (timer) clearInterval(timer);
    timer = null;
}
