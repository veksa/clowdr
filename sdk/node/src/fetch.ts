import {emitTrace} from './record';
import {maskSecrets, sanitize} from './sanitize';

// ─── Outbound fetch ───────────────────────────────────────────────────────────

let patched: typeof globalThis.fetch | null = null;
let unpatched: typeof globalThis.fetch | null = null;

/** The fetch in place before traceFetch() patched the global one. */
export function baseFetch(): typeof globalThis.fetch {
    return unpatched ?? globalThis.fetch;
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
        const {model, messages: _m, ...params} = parsed as Record<string, any>;
        return {
            model: typeof model === 'string' ? model : undefined,
            system: typeof system?.content === 'string' ? system.content : undefined,
            messages: messages.filter(m => m?.role !== 'system'),
            params,
        };
    } catch {
        return {};
    }
}

function parseResponse(p: any) {
    if (!p || typeof p !== 'object') return {};
    const choice = Array.isArray(p.choices) ? p.choices[0] : undefined;
    return {
        id: p.id as string | undefined,
        model: typeof p.model === 'string' ? (p.model as string) : undefined,
        text: typeof choice?.message?.content === 'string' ? (choice.message.content as string) : undefined,
        finishReason: choice?.finish_reason as string | undefined,
        usage: p.usage,
        // OpenRouter reports the charged amount in usage.cost.
        costUsd: typeof p.usage?.cost === 'number' ? (p.usage.cost as number) : undefined,
        error: p.error, // an error envelope with HTTP 200 happens
    };
}

/**
 * A `fetch` for an OpenAI-compatible model client (`createOpenAI({fetch: llmFetch})`,
 * `new OpenAI({fetch: llmFetch})`): records the exact prompt, the answer, token usage and cost
 * as one `llm` record. It bypasses the global patch, so a model call is not recorded twice.
 */
export const llmFetch: typeof globalThis.fetch = async (input, init) => {
    const started = Date.now();
    const url = typeof input === 'string' ? input : (input as any)?.url ?? String(input);
    const req = parseRequest(init?.body);
    const base = {type: 'llm', name: req.model ?? 'unknown-model', data: {url: stripQuery(url), system: req.system, messages: req.messages, params: req.params}};
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
        emitTrace({...base, ok: response.ok, level: response.ok ? undefined : 'error', durationMs, data: {...base.data, status: response.status, contentType}});
        return response;
    }
    let payload: any;
    try {
        payload = await response.clone().json();
    } catch {
        payload = undefined;
    }
    const res = parseResponse(payload);
    const ok = response.ok && !res.error;
    emitTrace({
        ...base,
        name: req.model ?? res.model ?? base.name,
        ok,
        level: ok ? undefined : 'error',
        durationMs,
        costUsd: res.costUsd,
        data: {...base.data, status: response.status, response: {id: res.id, text: res.text, finishReason: res.finishReason, error: res.error}, usage: res.usage},
    });
    return response;
};

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
