import {AsyncLocalStorage} from 'node:async_hooks';

/**
 * The ambient trace: which unit of work this is, for whom, and what started it. Carried in an
 * AsyncLocalStorage, so a record written deep inside a library's fetch, or in a fire-and-forget
 * `.then()`, still lands in the right trace without threading a context through every call.
 */
export interface TraceContext {
    traceId: string;
    startedAt: number;
    userId?: number | string;
    source?: string;
    /** Small facts about the whole unit of work (a Telegram user id, a tool name): written on every record. */
    tags?: Record<string, string | number | boolean>;
}

const storage = new AsyncLocalStorage<TraceContext>();

let counter = 0;

/**
 * Short, sortable id (it gets typed by hand). The parts are fixed width, so two ids made in one
 * millisecond can never read the same.
 */
export function newTraceId(): string {
    counter = (counter + 1) % 100000;
    const seq = counter.toString(36).padStart(4, '0');
    const rand = Math.floor(Math.random() * 1296).toString(36).padStart(2, '0');
    return `t${Date.now().toString(36)}${seq}${rand}`;
}

export function currentTrace(): TraceContext | undefined {
    return storage.getStore();
}

export function currentTraceId(): string | undefined {
    return storage.getStore()?.traceId;
}

export interface RunOptions {
    traceId?: string;
    userId?: number | string;
    /** What started the work: `http:GET /orders/12`, `job:invoice.send`. */
    source?: string;
    /** Tags of the trace (merged into the surrounding one when joining); undefined values are dropped. */
    tags?: Record<string, string | number | boolean | undefined>;
    /**
     * Start a new trace even inside another one: work kicked off by a request that outlives it
     * (a background job) is its own unit.
     */
    fresh?: boolean;
}

/**
 * Run `fn` inside a trace. Nested calls join the surrounding trace (filling in what it did not
 * know yet), so one request stays one trace; `fresh: true` starts a new one.
 */
export function runInTrace<T>(opts: RunOptions, fn: () => T): T {
    const existing = storage.getStore();
    if (existing && !opts.fresh) {
        if (opts.userId !== undefined && existing.userId === undefined) existing.userId = opts.userId;
        if (opts.source && !existing.source) existing.source = opts.source;
        if (opts.tags) existing.tags = {...existing.tags, ...defined(opts.tags)};
        return fn();
    }
    return storage.run({
        traceId: opts.traceId ?? newTraceId(),
        startedAt: Date.now(),
        userId: opts.userId,
        source: opts.source,
        tags: opts.tags ? defined(opts.tags) : undefined,
    }, fn);
}

/** Name what started the ambient trace once it is known (a socket frame decoded inside it). */
export function setTraceSource(source: string): void {
    const ctx = storage.getStore();
    if (ctx) ctx.source = source;
}

/** Stamp the user onto the ambient trace once it is known (in the auth guard). */
export function setTraceUser(userId: number | string | undefined | null): void {
    const ctx = storage.getStore();
    if (ctx && userId !== undefined && userId !== null) ctx.userId = userId;
}

/** Add tags to the ambient trace once they are known; written on every record after this. */
export function addTraceTags(tags: Record<string, string | number | boolean | undefined>): void {
    const ctx = storage.getStore();
    if (!ctx) return;
    ctx.tags = {...ctx.tags, ...defined(tags)};
}

function defined(tags: Record<string, string | number | boolean | undefined>): Record<string, string | number | boolean> {
    return Object.fromEntries(Object.entries(tags).filter(([, v]) => v !== undefined)) as Record<string, string | number | boolean>;
}
