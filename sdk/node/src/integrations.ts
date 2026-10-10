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
