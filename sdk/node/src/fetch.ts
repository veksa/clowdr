import {emitTrace} from './record';
import {maskSecrets, sanitize} from './sanitize';

// ─── Outbound fetch ───────────────────────────────────────────────────────────

let patched: typeof globalThis.fetch | null = null;
let unpatched: typeof globalThis.fetch | null = null;

/** The fetch in place before traceFetch() patched the global one (llmFetch goes through it). */
export function baseFetch(): typeof globalThis.fetch {
    return unpatched ?? globalThis.fetch;
}

/**
 * Replace what baseFetch() returns: for a layer that must also sit under model calls (rewriting
 * URLs, signing), installed after traceFetch(): `setBaseFetch(wrap(baseFetch()))`; null resets.
 */
export function setBaseFetch(fn: typeof globalThis.fetch | null): void {
    unpatched = fn;
}

/**
 * Record every bare `fetch(...)` in the process: method, url (query stripped), status, duration.
 * Patched at the global because call sites are many and the next one will not know about this.
 * Response bodies are never read: downloads go through fetch too.
 */
export function traceFetch(): void {
    if (patched && globalThis.fetch === patched) return; // a second wrap would record twice
    const native = globalThis.fetch.bind(globalThis);
    unpatched = native;
    patched = (async (input: any, init?: any) => {
        const started = Date.now();
        const url = typeof input === 'string' ? input : input?.url ?? String(input);
        const method = String(init?.method ?? input?.method ?? 'GET').toUpperCase();
        const name = `${method} ${hostAndPath(url)}`;
        try {
            const response = await native(input, init);
            emitTrace({
                type: 'http', name, ok: response.ok, level: response.ok ? undefined : 'warn',
                durationMs: Date.now() - started,
                data: {dir: 'out', url: stripQuery(url), status: response.status},
            });
            return response;
        } catch (e) {
            emitTrace({
                type: 'http', name, ok: false, level: 'error', durationMs: Date.now() - started,
                data: {dir: 'out', url: stripQuery(url), error: sanitize(e)},
            });
            throw e;
        }
    }) as typeof globalThis.fetch;
    globalThis.fetch = patched;
}

/** Put the original fetch back (tests). */
export function untraceFetch(): void {
    if (patched && globalThis.fetch === patched && unpatched) globalThis.fetch = unpatched;
    patched = null;
    unpatched = null;
}

// ─── Model calls ──────────────────────────────────────────────────────────────

function parseRequest(body: unknown) {
    try {
        const text = typeof body === 'string' ? body : body instanceof Uint8Array ? Buffer.from(body).toString('utf8') : undefined;
        if (text === undefined) return {};
        const parsed = JSON.parse(text);
        if (!parsed || typeof parsed !== 'object') return {};
        const messages: any[] = Array.isArray(parsed.messages) ? parsed.messages : [];
        const system = messages.find(m => m?.role === 'system');
        const {model, messages: _m, tools, ...params} = parsed as Record<string, any>;
        return {
            model: typeof model === 'string' ? model : undefined,
            system: typeof system?.content === 'string' ? system.content : undefined,
            messages: messages.filter(m => m?.role !== 'system'),
            // Names only: the schemas are the same on every call and would bury the prompt.
            tools: Array.isArray(tools) ? tools.map((t: any) => t?.function?.name ?? t?.name).filter(Boolean) : undefined,
            params,
        };
    } catch {
        return {};
    }
}

function parseArgs(args: unknown): unknown {
    if (typeof args !== 'string') return args;
    try {
        return JSON.parse(args);
    } catch {
        return args;
    }
}

function parseResponse(p: any) {
    if (!p || typeof p !== 'object') return {};
    const choice = Array.isArray(p.choices) ? p.choices[0] : undefined;
    const message = choice?.message;
    const toolCalls = Array.isArray(message?.tool_calls)
        ? message.tool_calls.map((c: any) => ({name: c?.function?.name ?? c?.name ?? 'unknown', args: parseArgs(c?.function?.arguments)}))
        : undefined;
    return {
        id: p.id as string | undefined,
        model: typeof p.model === 'string' ? (p.model as string) : undefined,
        text: typeof message?.content === 'string' ? (message.content as string) : undefined,
        reasoning: typeof message?.reasoning === 'string' ? (message.reasoning as string) : undefined,
        toolCalls: toolCalls?.length ? toolCalls : undefined,
        finishReason: choice?.finish_reason as string | undefined,
        usage: p.usage,
        // OpenRouter reports the charged amount in usage.cost.
        costUsd: typeof p.usage?.cost === 'number' ? (p.usage.cost as number) : undefined,
        error: p.error, // an error envelope with HTTP 200 happens
    };
}

function headerValue(headers: any, name: string): string | undefined {
    if (!headers) return undefined;
    const lower = name.toLowerCase();
    try {
        if (typeof headers.get === 'function') return headers.get(name) ?? undefined;
        if (Array.isArray(headers)) {
            const found = headers.find((pair: any) => String(pair?.[0]).toLowerCase() === lower);
            return typeof found?.[1] === 'string' ? found[1] : undefined;
        }
        for (const [key, value] of Object.entries(headers)) {
            if (key.toLowerCase() === lower && typeof value === 'string') return value;
        }
    } catch {
        // an exotic headers object: no route, still a record
    }
    return undefined;
}

const SKIP_FRAME = /[\\/]node_modules[\\/]|[\\/]clowdr-node[\\/]|\(node:|^\s*at node:/;

/**
 * The first application frame above the model client: `orders.service.ts:142`. Undefined in a
 * webpack bundle, where a line of main.js names nothing.
 */
export function callerFrame(): string | undefined {
    const stack = new Error().stack;
    if (!stack) return undefined;
    for (const line of stack.split('\n').slice(2)) {
        if (SKIP_FRAME.test(line)) continue;
        const m = line.match(/([\w.-]+\.(?:ts|js|mjs|cjs)):(\d+):\d+/);
        if (!m || /^main(\.[\w-]+)?\.js$/.test(m[1])) continue;
        return `${m[1]}:${m[2]}`;
    }
    return undefined;
}

export interface LlmFetchOptions {
    /**
     * A request header naming what the call is for ("dialogue", "summary"), set by the service on
     * its model client; recorded as data.route.
     */
    routeHeader?: string;
    /**
     * The cost of a call from the response's `usage`, for providers whose bill is not just
     * `usage.cost` (OpenRouter BYOK reports the upstream cost elsewhere). Return undefined when the
     * usage does not say: undefined means unknown, 0 means free. Default: `usage.cost` when a number.
     */
    cost?: (usage: any) => number | undefined;
}

/**
 * A `fetch` for an OpenAI-compatible model client (`createOpenAI({fetch})`, `new OpenAI({fetch})`):
 * records the exact prompt, the tools offered, the answer with its tool calls and reasoning, token
 * usage and cost as one `llm` record. It bypasses the global patch, so a model call is not
 * recorded twice. Streamed responses are recorded without their body.
 */
export function createLlmFetch(options: LlmFetchOptions = {}): typeof globalThis.fetch {
    return async (input, init) => {
        const started = Date.now();
        const url = typeof input === 'string' ? input : (input as any)?.url ?? String(input);
        const req = parseRequest(init?.body);
        const base = {
            type: 'llm',
            name: req.model ?? 'unknown-model',
            data: {
                url: stripQuery(url),
                route: options.routeHeader ? headerValue(init?.headers, options.routeHeader) : undefined,
                caller: callerFrame(),
                system: req.system,
                messages: req.messages,
                tools: req.tools,
                params: req.params,
            },
        };
        let response: Response;
        try {
            response = await baseFetch()(input as any, init as any);
        } catch (e) {
            emitTrace({...base, ok: false, level: 'error', durationMs: Date.now() - started, data: {...base.data, error: sanitize(e)}});
            throw e;
        }
        const durationMs = Date.now() - started;
        const contentType = response.headers?.get?.('content-type') ?? '';
        // Only JSON is safe to read: draining a stream would eat what the caller is about to read.
        if (!contentType.includes('application/json')) {
            emitTrace({...base, ok: response.ok, level: response.ok ? undefined : 'error', durationMs, data: {...base.data, status: response.status, contentType, streamed: true}});
            return response;
        }
        let payload: any;
        try {
            payload = await response.clone().json();
        } catch {
            payload = undefined;
        }
        const res = parseResponse(payload);
        let costUsd = res.costUsd;
        if (options.cost && res.usage) {
            try {
                costUsd = options.cost(res.usage);
            } catch {
                // a cost function that throws records no cost rather than no call
            }
        }
        const ok = response.ok && !res.error;
        emitTrace({
            ...base,
            name: req.model ?? res.model ?? base.name,
            ok,
            level: ok ? undefined : 'error',
            durationMs,
            costUsd,
            data: {
                ...base.data,
                status: response.status,
                response: {id: res.id, text: res.text, reasoning: res.reasoning, toolCalls: res.toolCalls, finishReason: res.finishReason, error: res.error},
                usage: res.usage,
            },
        });
        return response;
    };
}

/** createLlmFetch() without a route header. */
export const llmFetch: typeof globalThis.fetch = createLlmFetch();

/**
 * Wrap an AI SDK tool map so every execution records its arguments, result and duration as a
 * `tool` record: the model's answer names the tools it asked for, this is what they did. A tool
 * that quietly returned `{success: false}` looks like a success from outside.
 */
export function traceTools<T extends Record<string, any>>(tools: T): T {
    if (!tools) return tools;
    const out: Record<string, any> = {};
    for (const [name, def] of Object.entries(tools)) {
        if (!def || typeof def.execute !== 'function') {
            out[name] = def;
            continue;
        }
        const original = def.execute.bind(def);
        out[name] = {
            ...def,
            execute: async (args: any, opts: any) => {
                const started = Date.now();
                try {
                    const result = await original(args, opts);
                    emitTrace({type: 'tool', name, ok: true, durationMs: Date.now() - started, data: {args, result}});
                    return result;
                } catch (e) {
                    emitTrace({type: 'tool', name, ok: false, level: 'error', durationMs: Date.now() - started, data: {args, error: sanitize(e)}});
                    throw e;
                }
            },
        };
    }
    return out as T;
}

// ─── URLs ─────────────────────────────────────────────────────────────────────

/** `https://api.telegram.org/bot123:AA…/send?x=1` → `api.telegram.org/bot<redacted>/send`. */
export function hostAndPath(url: string): string {
    let out: string;
    try {
        const u = new URL(url);
        out = `${u.host}${u.pathname}`;
    } catch {
        out = url.split('?')[0];
    }
    return maskSecrets(out);
}

/** Query strings carry access tokens and presigned signatures. */
export function stripQuery(url: string): string {
    const cut = url.indexOf('?');
    return maskSecrets(cut < 0 ? url : `${url.slice(0, cut)}?<query>`);
}
