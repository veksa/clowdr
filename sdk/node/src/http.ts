import {currentTrace, runInTrace} from './context';
import {emitTrace, RESPONSE_MARK} from './record';

/** An error answer bigger than this is a page, not a message. */
const MAX_ERROR_BODY_CHARS = 4000;

/** Response header carrying the trace id, so a bug report can name the request. */
export const TRACE_ID_HEADER = 'X-Trace-Id';

export interface HttpTraceOptions {
    /**
     * Requests not recorded while they succeed: probes and scrapes. Default: `/health` and
     * `HEAD /`. A failing quiet request is still recorded.
     */
    quiet?: (method: string, path: string) => boolean;
    /** Requests never recorded at all. */
    skip?: (method: string, path: string) => boolean;
}

export function defaultQuiet(method: string, path: string): boolean {
    return path === '/health' || (method === 'HEAD' && (path === '/' || path === ''));
}

/**
 * Express-style middleware (NestJS: `app.use(...)` or `consumer.apply(...)`) that puts every
 * inbound request in its own trace. Mount it first, after the body parsers: it then wraps guards,
 * pipes, handlers and exception filters, so a request a guard rejected is recorded too, with the
 * database and cache calls the guard made. The status and, for an error, the body are read off
 * the response the client really got.
 */
export function httpTraceMiddleware(options: HttpTraceOptions = {}) {
    const quietFn = options.quiet ?? defaultQuiet;
    return (req: any, res: any, next: (err?: any) => void): void => {
        const url: string = req.originalUrl ?? req.url ?? '';
        const method: string = req.method ?? 'GET';
        const path = url.split('?')[0];
        if (options.skip?.(method, path)) {
            next();
            return;
        }
        const route = `${method} ${path}`;
        const quiet = quietFn(method, path);
        runInTrace({source: `http:${route}`, fresh: true}, () => {
            const started = Date.now();
            // Held rather than read at finish time: `finish` fires in whatever async context
            // flushed the write, and the response record would land in no trace. It is the live
            // context, so a user the guard resolves later is still seen.
            const ctx = currentTrace();
            if (ctx && typeof res.setHeader === 'function' && !res.headersSent) {
                res.setHeader(TRACE_ID_HEADER, ctx.traceId);
            }
            if (!quiet) {
                emitTrace({
                    type: 'http',
                    name: route,
                    // The path, never the raw url: the query goes as an object, so secret keys in it
                    // are redacted by name.
                    data: {
                        dir: 'in',
                        path,
                        query: nonEmpty(req.query),
                        body: method === 'GET' ? undefined : req.body,
                        ip: req.ip,
                        userAgent: req.headers?.['user-agent'],
                    },
                });
            }
            let errorBody: unknown;
            if (typeof res.send === 'function') {
                const send = res.send.bind(res);
                res.send = (body?: any) => {
                    if (res.statusCode >= 400) errorBody = parseErrorBody(body);
                    return send(body);
                };
            }
            let closed = false;
            const finish = () => {
                if (closed) return;
                closed = true;
                const status: number = res.statusCode;
                if (quiet && status < 400) return;
                emitTrace({
                    type: 'http',
                    name: `${route}${RESPONSE_MARK}`,
                    traceId: ctx?.traceId,
                    userId: ctx?.userId,
                    source: ctx?.source,
                    // A 4xx is an outcome (an expired token, a missing record), not a failure of
                    // the server: `trace ls --errors` stays the list of 5xx. It is a warning, so
                    // `trace errors` still shows it.
                    ok: status < 500,
                    level: status >= 500 ? 'error' : status >= 400 ? 'warn' : undefined,
                    durationMs: Date.now() - started,
                    data: {
                        dir: 'in',
                        status,
                        error: errorBody,
                        // The client hung up before the response was written.
                        aborted: res.writableEnded === false ? true : undefined,
                    },
                });
            };
            // `finish`: the response was written; `close`: the connection went first.
            res.once?.('finish', finish);
            res.once?.('close', finish);
            next();
        });
    };
}

function nonEmpty(v: unknown): unknown {
    return v && typeof v === 'object' && Object.keys(v).length > 0 ? v : undefined;
}

function parseErrorBody(body: unknown): unknown {
    if (typeof body !== 'string') return body;
    const text = body.length > MAX_ERROR_BODY_CHARS ? `${body.slice(0, MAX_ERROR_BODY_CHARS)}…` : body;
    try {
        return JSON.parse(text);
    } catch {
        return text;
    }
}
