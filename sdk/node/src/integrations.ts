import {hostAndPath, stripQuery} from './fetch';
import {channelEnabled, emitTrace} from './record';
import {sanitize} from './sanitize';

// ─── axios ────────────────────────────────────────────────────────────────────

/**
 * Record every request of an axios instance (pass the default one: `traceAxios(axios)`). On an
 * error the vendor's response body is recorded too: it says why the call failed. The
 * interceptors hand config and response back untouched and re-reject the original error.
 */
export function traceAxios(instance: any, label = 'axios'): void {
    if (!instance?.interceptors || instance.__clowdrTraced) return;
    instance.__clowdrTraced = true;
    instance.interceptors.request.use((config: any) => {
        config.__traceStartedAt = Date.now();
        return config;
    });
    const describe = (config: any) => {
        const url = absoluteUrl(config);
        return {
            name: `${String(config?.method ?? 'get').toUpperCase()} ${hostAndPath(url)}`,
            durationMs: config?.__traceStartedAt ? Date.now() - config.__traceStartedAt : undefined,
            url: stripQuery(url),
        };
    };
    instance.interceptors.response.use(
        (response: any) => {
            const {name, durationMs, url} = describe(response?.config);
            emitTrace({type: 'http', name, ok: true, durationMs, data: {dir: 'out', via: label, url, status: response?.status}});
            return response;
        },
        (error: any) => {
            const {name, durationMs, url} = describe(error?.config);
            emitTrace({
                type: 'http', name, ok: false, level: 'error', durationMs,
                data: {dir: 'out', via: label, url, status: error?.response?.status, body: error?.response?.data, error: sanitize(error?.message ?? error)},
            });
            return Promise.reject(error);
        },
    );
}

function absoluteUrl(config: any): string {
    const url = String(config?.url ?? '');
    const base = String(config?.baseURL ?? '');
    if (!base || /^https?:\/\//i.test(url)) return url;
    return `${base.replace(/\/$/, '')}/${url.replace(/^\//, '')}`;
}

// ─── S3 ───────────────────────────────────────────────────────────────────────

/**
 * Wrap an AWS SDK v3 S3 client so every command is recorded by what it did to which key. Bodies
 * are never recorded.
 */
export function traceS3<T extends object>(client: T): T {
    return new Proxy(client, {
        get(object, property, receiver) {
            const value = Reflect.get(object, property, receiver);
            if (typeof value !== 'function' || property !== 'send') return value;
            return async function (this: unknown, command: any, ...rest: any[]) {
                const op = String(command?.constructor?.name ?? 'Command').replace(/Command$/, '');
                const input = command?.input ?? {};
                const name = `${op} ${input.Key ?? ''}`.trim();
                const started = Date.now();
                try {
                    const result = await value.apply(object, [command, ...rest]);
                    emitTrace({type: 'storage', name, ok: true, durationMs: Date.now() - started, data: {bucket: input.Bucket}});
                    return result;
                } catch (e) {
                    emitTrace({type: 'storage', name, ok: false, level: 'error', durationMs: Date.now() - started, data: {bucket: input.Bucket, error: sanitize(e)}});
                    throw e;
                }
            };
        },
    });
}

// ─── Prisma driver adapter (channel `db`) ─────────────────────────────────────

/**
 * Record the SQL a Prisma driver adapter actually runs: `new PrismaClient({adapter:
 * tracePrismaAdapter(new PrismaPg(...))})`. Only while the `db` channel is on. Observational: the
 * query and the returned promise are never touched, handlers are only attached.
 */
export function tracePrismaAdapter<T extends {connect: () => Promise<any>}>(factory: T): T {
    const connect = factory.connect.bind(factory);
    factory.connect = async () => {
        const adapter = await connect();
        wrapQueryable(adapter, 'adapter');
        if (typeof adapter?.startTransaction === 'function') {
            const start = adapter.startTransaction.bind(adapter);
            adapter.startTransaction = async (...args: any[]) => {
                const tx = await start(...args);
                wrapQueryable(tx, 'transaction');
                return tx;
            };
        }
        return adapter;
    };
    return factory;
}

function wrapQueryable(target: any, via: string): void {
    if (!target) return;
    for (const method of ['queryRaw', 'executeRaw']) {
        const original = target[method];
        if (typeof original !== 'function') continue;
        const bound = original.bind(target);
        target[method] = (query: any) => {
            if (!channelEnabled('db')) return bound(query);
            const started = Date.now();
            const result = bound(query);
            if (!result || typeof result.then !== 'function') return result;
            const sql = String(query?.sql ?? '');
            // .then(ok, err) derives a new, always-fulfilled promise: no unhandled rejection.
            result.then(
                (value: any) => emitTrace({type: 'db', name: statementKind(sql), ok: true, durationMs: Date.now() - started, data: {via, sql, args: query?.args, rows: rowCount(value)}}),
                (error: any) => emitTrace({type: 'db', name: statementKind(sql), ok: false, level: 'error', durationMs: Date.now() - started, data: {via, sql, args: query?.args, error: sanitize(error)}}),
            );
            return result;
        };
    }
}

function rowCount(value: any): number | undefined {
    if (typeof value === 'number') return value;
    return Array.isArray(value?.rows) ? value.rows.length : undefined;
}

/** `SELECT … FROM "public"."orders" …` → `SELECT orders`. */
export function statementKind(sql: string): string {
    const verb = sql.trim().split(/\s+/, 1)[0]?.toUpperCase() || 'SQL';
    const table = sql.match(/(?:FROM|INTO|UPDATE)\s+(?:"?\w+"?\.)?"?(\w+)"?/i)?.[1];
    return table ? `${verb} ${table}` : verb;
}

// ─── Any call that leaves the process ─────────────────────────────────────────

export interface TracedCall {
    type: string;
    /** What was called: a URL, a key, a template. The record's title. */
    name: string;
    /** Kept as is (after sanitising): what identifies the call. */
    data?: Record<string, any>;
}

/**
 * Wrap one call the global fetch patch cannot see (an SDK with its own HTTP stack, a mail
 * provider, a queue): one record when it finishes, with its duration and outcome. A call that
 * never returns leaves no record.
 */
export async function traceCall<T>(call: TracedCall, run: () => Promise<T>, describe?: (result: T) => Record<string, any> | undefined): Promise<T> {
    const started = Date.now();
    try {
        const result = await run();
        emitTrace({type: call.type, name: call.name, ok: true, durationMs: Date.now() - started, data: {...call.data, ...(describe?.(result) ?? {})}});
        return result;
    } catch (error) {
        emitTrace({type: call.type, name: call.name, ok: false, level: 'error', durationMs: Date.now() - started, data: {...call.data, error: sanitize(error)}});
        throw error;
    }
}

// ─── Redis (channel `cache`) ──────────────────────────────────────────────────

const REDIS_COMMANDS = ['get', 'set', 'del', 'getdel', 'incr', 'expire', 'ttl', 'exists', 'sadd', 'srem', 'smembers', 'zadd', 'zrevrange', 'zrange', 'hget', 'hset', 'hdel', 'hgetall', 'mget'];
const REDIS_READS = new Set(['get', 'getdel', 'ttl', 'exists', 'smembers', 'zrevrange', 'zrange', 'hget', 'hgetall', 'mget']);

/**
 * Record ioredis commands while the `cache` channel is on: `traceRedis(new Redis(...))`. Reads
 * report whether the key was there. Arguments and the returned promise are passed through
 * untouched; pipelines (`multi()`) are not recorded.
 */
export function traceRedis<T extends object>(client: T, commands: string[] = REDIS_COMMANDS): T {
    const allowed = new Set(commands);
    return new Proxy(client, {
        get(object, property, receiver) {
            const value: unknown = Reflect.get(object, property, receiver);
            const command = String(property);
            if (typeof value !== 'function' || !allowed.has(command)) return value;
            const original = value as (...args: any[]) => any;
            return function (...args: any[]) {
                if (!channelEnabled('cache')) return original.apply(object, args);
                const started = Date.now();
                const result = original.apply(object, args);
                if (!result || typeof result.then !== 'function') return result;
                result.then(
                    (payload: any) => emitTrace({type: 'cache', name: `${command} ${args[0]}`, ok: true, durationMs: Date.now() - started, data: REDIS_READS.has(command) ? {hit: payload !== null && payload !== undefined} : {}}),
                    (error: any) => emitTrace({type: 'cache', name: `${command} ${args[0]}`, ok: false, level: 'error', durationMs: Date.now() - started, data: {error: sanitize(error)}}),
                );
                return result;
            };
        },
    });
}

// ─── Prisma query events (channel `db`) ───────────────────────────────────────

/**
 * Record SQL from Prisma's own query event, for clients without a driver adapter to wrap:
 * `prisma.$on('query', tracePrismaQuery)` with `log: [{emit: 'event', level: 'query'}]`.
 */
export function tracePrismaQuery(event: {query: string; params?: string; duration?: number}): void {
    if (!channelEnabled('db')) return;
    emitTrace({
        type: 'db',
        name: statementKind(event.query),
        durationMs: event.duration === undefined ? undefined : Math.round(event.duration * 100) / 100,
        data: {sql: event.query, params: event.params},
    });
}

// ─── node-postgres pool (channel `db`) ────────────────────────────────────────

function describeQuery(args: any[]): {sql: string; values?: unknown} {
    const first = args[0];
    if (typeof first === 'string') return {sql: first, values: args[1]};
    if (first && typeof first === 'object') return {sql: typeof first.text === 'string' ? first.text : '<not a string>', values: first.values ?? args[1]};
    return {sql: '<unknown query shape>'};
}

/**
 * Record the SQL a `pg` Pool (or Client) runs while the `db` channel is on: `tracePgPool(pool)`,
 * for a pool handed to an adapter that offers nothing to wrap. Clients checked out with
 * `pool.connect()` (transactions) are wrapped too. Observational: the query and its promise or
 * callback are passed through untouched.
 */
export function tracePgPool<T extends {query: (...args: any[]) => any; connect?: (...args: any[]) => any}>(pool: T): T {
    wrapPgQuery(pool, 'pool');
    if (typeof pool.connect === 'function') {
        const connect = pool.connect.bind(pool);
        pool.connect = function (...args: any[]) {
            if (typeof args[0] === 'function') return connect(...args); // callback form: untouched
            const result = connect(...args);
            if (!result || typeof result.then !== 'function') return result;
            return result.then((client: any) => {
                if (client && !client.__clowdrTraced) {
                    client.__clowdrTraced = true;
                    wrapPgQuery(client, 'client');
                }
                return client;
            });
        } as T['connect'];
    }
    return pool;
}

function wrapPgQuery(target: {query: (...args: any[]) => any}, via: string): void {
    if (typeof target.query !== 'function') return;
    const original = target.query.bind(target);
    target.query = function tracedQuery(...args: any[]) {
        if (!channelEnabled('db')) return original(...args);
        const started = Date.now();
        const {sql, values} = describeQuery(args);
        const record = (extra: Record<string, any>, failed = false) => emitTrace({
            type: 'db', name: statementKind(sql), ok: !failed, level: failed ? 'error' : undefined,
            durationMs: Date.now() - started, data: {via, sql, values, ...extra},
        });
        let result: any;
        try {
            result = original(...args);
        } catch (error) {
            record({error: sanitize(error)}, true);
            throw error;
        }
        if (typeof args[args.length - 1] === 'function' || !result || typeof result.then !== 'function') {
            record({});
            return result;
        }
        result.then(
            (rows: any) => record({rows: rows?.rowCount ?? rows?.rows?.length}),
            (error: unknown) => record({error: sanitize(error)}, true),
        );
        return result;
    };
}

// ─── Any SDK client ───────────────────────────────────────────────────────────

/**
 * Record every call to the named methods of an object (a payment provider, a mail SDK), arguments
 * and result included. A Proxy keeps the object otherwise identical. `pickArgs` keeps what is worth
 * recording when the arguments are bulky (rendered HTML).
 */
export function traceMethods<T extends object>(target: T, options: {type: string; label: string; methods: string[]; pickArgs?: (args: any[]) => any}): T {
    const {type, label, methods, pickArgs} = options;
    return new Proxy(target, {
        get(object, property, receiver) {
            const value = Reflect.get(object, property, receiver);
            if (typeof value !== 'function' || !methods.includes(String(property))) return value;
            return function (...args: any[]) {
                const name = `${label}.${String(property)}`;
                const started = Date.now();
                const write = (ok: boolean, data: Record<string, any>) => emitTrace({
                    type, name, ok, level: ok ? undefined : 'error', durationMs: Date.now() - started,
                    data: {args: pickArgs ? pickArgs(args) : args, ...data},
                });
                let result: any;
                try {
                    // On the target, not the proxy: a method calling a sibling would re-enter the trap.
                    result = value.apply(object, args);
                } catch (error) {
                    write(false, {error: sanitize(error)});
                    throw error;
                }
                if (!result || typeof result.then !== 'function') {
                    write(true, {result});
                    return result;
                }
                return result.then(
                    (v: any) => {
                        write(true, {result: v});
                        return v;
                    },
                    (error: any) => {
                        write(false, {error: sanitize(error)});
                        throw error;
                    },
                );
            };
        },
    });
}
