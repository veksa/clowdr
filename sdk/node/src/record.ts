import {currentTrace} from './context';
import {maskSecrets, sanitize} from './sanitize';

export type RecordType =
    | 'http'    // an inbound request (data.dir 'in') or an outgoing call (data.dir 'out')
    | 'llm'     // one round-trip to a model
    | 'job'     // start / outcome of a unit of background work
    | 'log'     // one log line
    | 'storage' // one object-storage operation
    | 'db'      // one SQL statement (channel `db`)
    | 'note'    // an explicit marker
    | 'socket'; // one frame on a websocket

export type Level = 'debug' | 'info' | 'warn' | 'error';

export interface RecordInput {
    type: RecordType | string;
    name: string;
    traceId?: string;
    userId?: number | string;
    source?: string;
    level?: Level;
    durationMs?: number;
    ok?: boolean;
    costUsd?: number;
    data?: Record<string, any>;
}

/** Trace id of records written outside any unit of work (boot, timers). */
export const ORPHAN_TRACE = '-';

/** Ends the name of the record of an inbound request's response. */
export const RESPONSE_MARK = ' ←';

// A record is one stdout line; clowdr stores lines up to 16 MB. Past this budget the data is
// re-sanitised with shorter strings, and past it again replaced by a marker.
const MAX_RECORD_BYTES = 1 << 20;

const boot = `${Date.now().toString(36)}${Math.floor(Math.random() * 46656).toString(36)}`;
let seq = 0;
let writing = false;
let enabled = process.env.CLOWDR_TRACE !== '0';
let write: (line: string) => void = line => {
    process.stdout.write(line);
};

/** Turn recording off (`CLOWDR_TRACE=0` does the same at start). */
export function setTracing(on: boolean): void {
    enabled = on;
}

export function isTracing(): boolean {
    return enabled;
}

/** Where lines go; for tests. */
export function setTraceWriter(fn: ((line: string) => void) | null): void {
    write = fn ?? (line => {
        process.stdout.write(line);
    });
}

/**
 * Write one record. Never throws and never recurses: a broken recorder must not take down the
 * request it observes.
 */
export function emitTrace(input: RecordInput): void {
    if (!enabled || writing) return;
    writing = true;
    try {
        const ctx = currentTrace();
        const record: Record<string, any> = {
            // "$" first: clowdr routes a stdout line by this prefix without parsing it.
            $: 'trace',
            seq: seq++,
            boot,
            ts: new Date().toISOString(),
            type: input.type,
            traceId: input.traceId ?? ctx?.traceId ?? ORPHAN_TRACE,
            userId: input.userId ?? ctx?.userId,
            source: input.source ?? ctx?.source,
            // Names carry URLs and log contexts, where tokens hide.
            name: maskSecrets(String(input.name)),
            level: input.level,
            durationMs: input.durationMs,
            ok: input.ok,
            costUsd: input.costUsd,
            data: input.data ? sanitize(input.data) : undefined,
        };
        let line = JSON.stringify(record);
        if (line.length > MAX_RECORD_BYTES && input.data) {
            record.data = sanitize(input.data, 2_000);
            line = JSON.stringify(record);
            if (line.length > MAX_RECORD_BYTES) {
                record.data = {truncated: `<record over ${MAX_RECORD_BYTES} bytes>`};
                line = JSON.stringify(record);
            }
        }
        write(line + '\n');
    } catch {
        // Tracing is best-effort by construction.
    } finally {
        writing = false;
    }
}

/** A marker in the timeline: "the reproduction starts here". */
export function traceNote(name: string, data?: Record<string, any>): void {
    emitTrace({type: 'note', name, data});
}

/** Start or outcome of a unit of background work: the IN / OUT lines of a job trace. */
export function traceJob(name: string, data: Record<string, any>, opts?: {ok?: boolean; durationMs?: number; level?: Level}): void {
    emitTrace({
        type: 'job',
        name,
        ok: opts?.ok,
        level: opts?.level ?? (opts?.ok === false ? 'error' : undefined),
        durationMs: opts?.durationMs,
        data,
    });
}

/**
 * One log line into the trace. Hook it into the service's logger next to its console output:
 * the plain line stays in `clowctl logs`, this one shows inside `clowctl trace show`.
 */
export function traceLog(level: Level, context: string | undefined, message: string): void {
    emitTrace({type: 'log', name: context || 'log', level, data: {message}});
}

// ─── Channels ─────────────────────────────────────────────────────────────────
// Kinds of records too noisy to write always: one job is dozens of SQL statements. Off by default,
// switched on per deployment: `clowctl env set server CLOWDR_TRACE_CHANNELS=db` and a restart.

const channels = new Set((process.env.CLOWDR_TRACE_CHANNELS ?? '').split(',').map(s => s.trim()).filter(Boolean));

export function channelEnabled(name: string): boolean {
    return channels.has(name);
}

export function setChannel(name: string, on: boolean): void {
    if (on) channels.add(name);
    else channels.delete(name);
}
