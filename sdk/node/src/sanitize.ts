// Everything that goes into a record passes through here first:
//  1. serialisable: BigInt, Decimal, Dates, Buffers, Errors and cycles must not make
//     JSON.stringify throw inside a logging path;
//  2. bounded: data URLs and whole documents would make one record megabytes;
//  3. without secrets: a key in a trace is a key in the server's log store.

export const MAX_STRING_CHARS = 20_000;
const MAX_ARRAY_ITEMS = 200;
const MAX_DEPTH = 8;
/** Beyond this, a single-token string is a blob, not text. */
const BLOB_THRESHOLD = 512;

// Suffix patterns rather than today's names: the next credential field will be written by
// somebody who never read this file. Singular `token` on purpose: `prompt_tokens` is a count.
const SECRET_KEY_RE = /^(authorization|cookie|set-cookie|key|signature|x-signature|[\w-]*(token|api[-_]?key|secret|secret[-_]?key|password|passwd|private[-_]?key|encrypted[-_]?key))$/i;

const SECRET_VALUE_RES: RegExp[] = [
    /sk-or-v1-[A-Za-z0-9]{16,}/g, // OpenRouter
    /sk-[A-Za-z0-9_-]{32,}/g, // OpenAI-style keys
    /\d{8,12}:AA[A-Za-z0-9_-]{30,}/g, // Telegram bot token, also inside Bot API URLs
    /eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g, // JWT
    /Bearer\s+[A-Za-z0-9._:-]{8,}/gi,
    /EAA[A-Za-z0-9]{40,}/g, // Meta Graph API
    /AKIA[0-9A-Z]{16}/g, // AWS access key id
];

const extraKeys: RegExp[] = [];

/** Teach the sanitiser a service's own secret shapes: a field name or a value pattern. */
export function addSecretPattern(p: {key?: RegExp; value?: RegExp}): void {
    if (p.key) extraKeys.push(p.key);
    if (p.value) SECRET_VALUE_RES.push(new RegExp(p.value.source, p.value.flags.includes('g') ? p.value.flags : p.value.flags + 'g'));
}

const BASE64_RE = /^[A-Za-z0-9+/\s]{512,}={0,2}$/;

export function maskSecrets(value: string): string {
    let out = value;
    for (const re of SECRET_VALUE_RES) out = out.replace(re, '<redacted>');
    return out;
}

function secretKey(key: string): boolean {
    return SECRET_KEY_RE.test(key) || extraKeys.some(re => re.test(key));
}

/**
 * Clip a long string keeping BOTH ends: the cause of a failure is often on the last line of a
 * long message.
 */
export function clip(text: string, maxChars: number): string {
    if (text.length <= maxChars) return text;
    const tailChars = Math.min(2_000, Math.floor(maxChars / 4));
    const headChars = Math.max(0, maxChars - tailChars);
    const dropped = text.length - headChars - tailChars;
    return `${text.slice(0, headChars)}…[+${dropped} chars]…${tailChars > 0 ? text.slice(text.length - tailChars) : ''}`;
}

function sanitiseString(value: string, maxChars: number): string {
    if (value.startsWith('data:') && value.length > BLOB_THRESHOLD) {
        const mime = value.slice(5, Math.min(value.indexOf(',') + 1 || 40, 60));
        return `<data-uri ${mime} ${value.length} chars>`;
    }
    if (value.length > BLOB_THRESHOLD && BASE64_RE.test(value)) return `<base64 ${value.length} chars>`;
    return clip(maskSecrets(value), maxChars);
}

/** Deep-copy `value` into something safe to write as JSON. Never throws. */
export function sanitize(value: unknown, maxChars: number = MAX_STRING_CHARS): any {
    return walk(value, maxChars, 0, new WeakSet());
}

function walk(value: any, maxChars: number, depth: number, seen: WeakSet<object>): any {
    if (value === null || value === undefined) return null;
    const t = typeof value;
    if (t === 'string') return sanitiseString(value, maxChars);
    if (t === 'number' || t === 'boolean') return value;
    if (t === 'bigint') return `${value.toString()}n`;
    if (t === 'function') return `<function ${value.name || 'anonymous'}>`;
    if (t === 'symbol') return String(value);
    if (value instanceof Date) return isNaN(value.getTime()) ? '<invalid date>' : value.toISOString();
    if (value instanceof Error) {
        return {
            name: value.name,
            message: sanitiseString(value.message ?? '', maxChars),
            stack: value.stack ? sanitiseString(value.stack, 2000) : undefined,
        };
    }
    if (Buffer.isBuffer(value)) return `<Buffer ${value.length} bytes>`;
    if (ArrayBuffer.isView(value)) return `<${value.constructor?.name ?? 'TypedArray'} ${value.byteLength} bytes>`;
    if (value instanceof ArrayBuffer) return `<ArrayBuffer ${value.byteLength} bytes>`;
    if (depth >= MAX_DEPTH) return '<max depth>';
    if (seen.has(value)) return '<circular>';
    seen.add(value);
    try {
        if (Array.isArray(value)) {
            const items = value.slice(0, MAX_ARRAY_ITEMS).map(v => walk(v, maxChars, depth + 1, seen));
            if (value.length > MAX_ARRAY_ITEMS) items.push(`<+${value.length - MAX_ARRAY_ITEMS} more items>`);
            return items;
        }
        if (value instanceof Map) return walk(Object.fromEntries(value), maxChars, depth, seen);
        if (value instanceof Set) return walk([...value], maxChars, depth, seen);
        // Decimal and anything else that describes itself in one value.
        if (typeof value.toJSON === 'function') {
            const plain = value.toJSON();
            if (typeof plain !== 'object' || plain === null) return walk(plain, maxChars, depth, seen);
        }
        const out: Record<string, any> = {};
        for (const [key, val] of Object.entries(value)) {
            out[key] = secretKey(key) ? '<redacted>' : walk(val, maxChars, depth + 1, seen);
        }
        return out;
    } catch (e) {
        return `<unserialisable ${(e as Error)?.message ?? ''}>`;
    } finally {
        seen.delete(value);
    }
}
