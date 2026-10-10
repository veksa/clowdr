import {runInTrace} from './context';
import {emitTrace, RESPONSE_MARK} from './record';

/** An error answer bigger than this is a page, not a message. */
const MAX_ERROR_BODY_CHARS = 4000;

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
 * Express-style middleware (works in NestJS through `app.use(...)` or `consumer.apply(...)`) that
 * puts every inbound request in its own trace. Mount it first: it then wraps guards, pipes,
 * handlers and exception filters, so a request a guard rejected is recorded too. The status and,
 * for an error, the body are read off the response the client really got.
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
            if (!quiet) {
                emitTrace({
                    type: 'http',
                    name: route,
                    // The path, never the raw url: the query goes as an object, so secret keys in it
                    // are redacted by name.
                    data: {dir: 'in', path, query: req.query, body: method === 'GET' ? undefined : req.body, ip: req.ip},
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
            res.once?.('finish', () => {
                const ok = res.statusCode < 400;
                if (quiet && ok) return;
                emitTrace({
                    type: 'http',
                    name: `${route}${RESPONSE_MARK}`,
                    ok,
                    level: res.statusCode >= 500 ? 'error' : ok ? undefined : 'warn',
                    durationMs: Date.now() - started,
                    data: {dir: 'in', status: res.statusCode, error: errorBody},
                });
            });
            next();
        });
    };
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
