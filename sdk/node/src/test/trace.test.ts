import {test} from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {
    emitTrace, httpTraceMiddleware, llmFetch, runInTrace, setChannel, setTraceUser, setTraceWriter, setTracing,
    traceFetch, traceJob, untraceFetch, channelEnabled, traceS3,
} from '../index';

function capture(): {records: any[]; lines: string[]} {
    const out = {records: [] as any[], lines: [] as string[]};
    setTracing(true);
    setTraceWriter(line => {
        out.lines.push(line);
        out.records.push(JSON.parse(line));
    });
    return out;
}

test('a record is one stdout line starting with the clowdr trace mark', () => {
    const c = capture();
    emitTrace({type: 'note', name: 'hello', data: {a: 1}});
    assert.equal(c.lines.length, 1);
    assert.ok(c.lines[0].startsWith('{"$":"trace"'), c.lines[0]);
    assert.ok(c.lines[0].endsWith('\n'));
    assert.equal(c.records[0].traceId, '-');
});

test('records inside runInTrace share its trace; nested calls join, fresh ones do not', () => {
    const c = capture();
    runInTrace({source: 'job:a'}, () => {
        setTraceUser(7);
        emitTrace({type: 'note', name: 'one'});
        runInTrace({source: 'ignored'}, () => emitTrace({type: 'note', name: 'two'}));
        runInTrace({source: 'job:b', fresh: true}, () => emitTrace({type: 'note', name: 'three'}));
    });
    const [one, two, three] = c.records;
    assert.equal(one.traceId, two.traceId);
    assert.equal(two.source, 'job:a');
    assert.equal(one.userId, 7);
    assert.notEqual(three.traceId, one.traceId);
    assert.equal(three.source, 'job:b');
});

test('secrets are redacted by key and by shape; huge records are cut to fit a line', () => {
    const c = capture();
    emitTrace({
        type: 'http', name: 'POST api.telegram.org/bot123456789:AAabcdefghijklmnopqrstuvwxyz0123456/send',
        data: {password: 'p', accessToken: 'x', prompt_tokens: 12, note: 'Bearer abcdefghijkl', n: BigInt(5)},
    });
    const r = c.records[0];
    assert.match(r.name, /bot<redacted>\/send/);
    assert.equal(r.data.password, '<redacted>');
    assert.equal(r.data.accessToken, '<redacted>');
    assert.equal(r.data.prompt_tokens, 12);
    assert.equal(r.data.note, '<redacted>');
    assert.equal(r.data.n, '5n');

    const big = Array.from({length: 150}, () => 'x '.repeat(10_000));
    emitTrace({type: 'note', name: 'big', data: {big}});
    assert.ok(c.lines[1].length < 1 << 20, String(c.lines[1].length));
});

test('jobs, channels', () => {
    const c = capture();
    traceJob('invoice.send', {id: 1}, {ok: false});
    assert.equal(c.records[0].level, 'error');
    assert.equal(channelEnabled('db'), false);
    setChannel('db', true);
    assert.equal(channelEnabled('db'), true);
    setChannel('db', false);
});

test('fetch is recorded once; llmFetch records the prompt and the answer', async () => {
    const c = capture();
    const original = globalThis.fetch;
    globalThis.fetch = (async (input: any) => {
        if (String(input).includes('/chat')) {
            return new Response(JSON.stringify({model: 'm', choices: [{message: {content: 'hi there'}, finish_reason: 'stop'}], usage: {prompt_tokens: 3, cost: 0.01}}), {headers: {'content-type': 'application/json'}});
        }
        return new Response('nope', {status: 503});
    }) as any;
    try {
        traceFetch();
        traceFetch(); // idempotent
        await fetch('https://api.example.com/x?token=secret');
        await llmFetch('https://llm.example.com/chat', {method: 'POST', body: JSON.stringify({model: 'm', messages: [{role: 'system', content: 's'}, {role: 'user', content: 'hello'}]})});
    } finally {
        untraceFetch();
        globalThis.fetch = original;
    }
    assert.equal(c.records.length, 2);
    const [http, llm] = c.records;
    assert.equal(http.type, 'http');
    assert.equal(http.ok, false);
    assert.equal(http.data.url, 'https://api.example.com/x?<query>');
    assert.equal(llm.type, 'llm');
    assert.equal(llm.data.system, 's');
    assert.equal(llm.data.response.text, 'hi there');
    assert.equal(llm.costUsd, 0.01);
});

test('http middleware: one trace per request, quiet probes, error bodies', () => {
    const c = capture();
    const mw = httpTraceMiddleware();
    const headers: Record<string, string> = {};
    const request = (method: string, url: string, status: number, body?: string) => {
        const res: any = Object.assign(new EventEmitter(), {
            statusCode: 200, writableEnded: true,
            send(b: any) { return b; },
            setHeader(k: string, v: string) { headers[k] = v; },
        });
        mw({method, originalUrl: url, query: {t: 'x'}, body: {a: 1}}, res, () => {
            emitTrace({type: 'note', name: 'inside'});
            setTraceUser(5);
            res.statusCode = status;
            if (body) res.send(body);
        });
        // Emitted outside the request's async context, like a socket flush, and twice (finish +
        // close): still one response record, in the request's trace.
        res.emit('finish');
        res.emit('close');
    };
    request('POST', '/orders?x=1', 400, '{"message":"bad"}');
    const firstTraceHeader = headers['X-Trace-Id'];
    request('GET', '/health', 200);
    request('GET', '/health', 503);
    const [inbound, inside, out] = c.records;
    assert.equal(inbound.name, 'POST /orders');
    assert.equal(inbound.source, 'http:POST /orders');
    assert.equal(inside.traceId, inbound.traceId);
    assert.equal(out.name, 'POST /orders ←');
    assert.equal(out.traceId, inbound.traceId);
    assert.equal(out.userId, 5);
    assert.equal(firstTraceHeader, inbound.traceId);
    // A 4xx is an outcome: ok, but a warning.
    assert.equal(out.ok, true);
    assert.equal(out.level, 'warn');
    assert.deepEqual(out.data.error, {message: 'bad'});
    assert.equal(c.records.filter(r => r.name === 'POST /orders ←').length, 1);
    // A healthy probe writes no request records; a failing one writes its response.
    const health = c.records.filter(r => r.type === 'http' && r.name.startsWith('GET /health'));
    assert.equal(health.length, 1);
    assert.equal(health[0].name, 'GET /health ←');
    assert.equal(health[0].data.status, 503);
});

test('S3 commands are recorded by operation and key', async () => {
    const c = capture();
    class PutObjectCommand { constructor(public input: any) {} }
    const client = traceS3({send: async (cmd: any) => ({ok: cmd.input.Key})});
    await client.send(new PutObjectCommand({Bucket: 'b', Key: 'a/b.png'}));
    assert.equal(c.records[0].name, 'PutObject a/b.png');
    assert.equal(c.records[0].data.bucket, 'b');
});

test('a log line without a context is named by its leading [Tag]', async () => {
    const {traceLog} = await import('../index');
    const c = capture();
    traceLog('error', undefined, '[Billing] step failed');
    traceLog('info', undefined, 'plain');
    traceLog('warn', 'Mail', '[Other] x');
    assert.deepEqual(c.records.map(r => r.name), ['Billing', 'app', 'Mail']);
});

test('traceCall, traceRedis and tracePrismaQuery', async () => {
    const {traceCall, traceRedis, tracePrismaQuery} = await import('../index');
    const c = capture();
    assert.equal(await traceCall({type: 'mail', name: 'welcome', data: {to: 'a@example.com'}}, async () => 42, n => ({id: n})), 42);
    await assert.rejects(traceCall({type: 'mail', name: 'bounce'}, async () => { throw new Error('no'); }));
    const redis = traceRedis({get: async (k: string) => (k === 'hit' ? '1' : null), set: async (_k: string, _v: string) => 'OK', other: () => 1});
    await redis.get('hit'); // channel off: nothing
    setChannel('cache', true);
    setChannel('db', true);
    try {
        await redis.get('hit');
        await redis.get('miss');
        await redis.set('k', 'v');
        await new Promise(r => setImmediate(r));
        tracePrismaQuery({query: 'SELECT * FROM "s"."orders" WHERE id=$1', params: '[1]', duration: 1.23456});
    } finally {
        setChannel('cache', false);
        setChannel('db', false);
    }
    const names = c.records.map(r => `${r.type}:${r.name}:${r.ok}`);
    assert.deepEqual(names, ['mail:welcome:true', 'mail:bounce:false', 'cache:get hit:true', 'cache:get miss:true', 'cache:set k:true', 'db:SELECT orders:undefined']);
    assert.equal(c.records[0].data.id, 42);
    assert.equal(c.records[2].data.hit, true);
    assert.equal(c.records[3].data.hit, false);
    assert.equal(c.records[5].durationMs, 1.23);
});

test('createLlmFetch records the route, the tools offered and the tool calls', async () => {
    const {createLlmFetch} = await import('../index');
    const c = capture();
    const original = globalThis.fetch;
    globalThis.fetch = (async () => new Response(JSON.stringify({
        choices: [{message: {content: null, reasoning: 'think', tool_calls: [{function: {name: 'lookup', arguments: '{"id":7}'}}]}, finish_reason: 'tool_calls'}],
    }), {headers: {'content-type': 'application/json'}})) as any;
    try {
        const f = createLlmFetch({routeHeader: 'x-route'});
        await f('https://llm.example.com/chat', {
            method: 'POST', headers: {'X-Route': 'dialogue'},
            body: JSON.stringify({model: 'm', messages: [{role: 'user', content: 'hi'}], tools: [{type: 'function', function: {name: 'lookup', parameters: {}}}]}),
        });
    } finally {
        globalThis.fetch = original;
    }
    const r = c.records[0];
    assert.equal(r.data.route, 'dialogue');
    assert.deepEqual(r.data.tools, ['lookup']);
    assert.deepEqual(r.data.response.toolCalls, [{name: 'lookup', args: {id: 7}}]);
    assert.equal(r.data.response.reasoning, 'think');
    assert.equal(r.data.params.tools, undefined);
});

test('traceTools and tracePgPool', async () => {
    const {traceTools, tracePgPool} = await import('../index');
    const c = capture();
    const tools: any = traceTools({ok: {execute: async (a: any, _o: any) => ({n: a.n + 1})}, bad: {execute: async (_a: any, _o: any) => { throw new Error('x'); }}, plain: {description: 'no execute'}});
    assert.deepEqual(await tools.ok.execute({n: 1}, {}), {n: 2});
    await assert.rejects(tools.bad.execute({}, {}));
    assert.equal((tools as any).plain.description, 'no execute');
    const pool = tracePgPool({query: async (_sql: string, _v?: unknown[]) => ({rowCount: 3})});
    setChannel('db', true);
    try {
        await pool.query('UPDATE "s"."orders" SET x=$1', [1]);
        await new Promise(r => setImmediate(r));
    } finally {
        setChannel('db', false);
    }
    assert.deepEqual(c.records.map(r => `${r.type}:${r.name}:${r.ok}`), ['tool:ok:true', 'tool:bad:false', 'db:UPDATE orders:true']);
    assert.equal(c.records[2].data.rows, 3);
});

test('tracePgPool wraps checked-out clients; traceMethods records calls', async () => {
    const {tracePgPool, traceMethods} = await import('../index');
    const c = capture();
    const client = {query: async () => ({rows: [1, 2]})};
    const pool = tracePgPool({query: async () => ({rowCount: 0}), connect: async () => client});
    setChannel('db', true);
    try {
        const cl: any = await pool.connect();
        await cl.query({text: 'INSERT INTO "s"."t" VALUES ($1)', values: [1]});
        await new Promise(r => setImmediate(r));
    } finally {
        setChannel('db', false);
    }
    const pay = traceMethods({charge: async (n: number) => ({id: n}), other: () => 1}, {type: 'payment', label: 'stripe', methods: ['charge']});
    await pay.charge(5);
    pay.other();
    assert.deepEqual(c.records.map(r => `${r.type}:${r.name}:${r.data.via ?? ''}`), ['db:INSERT t:client', 'payment:stripe.charge:']);
    assert.equal(c.records[0].data.rows, 2);
    assert.deepEqual(c.records[1].data.result, {id: 5});
});

test('onTraceRecord hears records even with tracing off', async () => {
    const {onTraceRecord, setTracing, emitTrace, runInTrace} = await import('../index');
    const c = capture();
    setTracing(false);
    const seen: Array<[number | undefined, unknown]> = [];
    const off = onTraceRecord((r, ctx) => {
        if (r.costUsd) seen.push([r.costUsd, ctx?.userId]);
        throw new Error('ignored');
    });
    try {
        runInTrace({userId: 9}, () => emitTrace({type: 'llm', name: 'm', costUsd: 0.5}));
        emitTrace({type: 'log', name: 'x'});
    } finally {
        off();
        setTracing(true);
    }
    assert.deepEqual(seen, [[0.5, 9]]);
    assert.equal(c.records.length, 0);
});

test('trace tags land on every record of the trace', async () => {
    const {runInTrace, addTraceTags, emitTrace} = await import('../index');
    const c = capture();
    runInTrace({source: 'telegram:message', tags: {telegramUserId: '42', skip: undefined}, fresh: true}, () => {
        emitTrace({type: 'note', name: 'a'});
        addTraceTags({tool: 'get_memory'});
        emitTrace({type: 'note', name: 'b'});
    });
    emitTrace({type: 'note', name: 'outside'});
    assert.deepEqual(c.records.map(r => r.tags), [{telegramUserId: '42'}, {telegramUserId: '42', tool: 'get_memory'}, undefined]);
});

test('createLlmFetch takes the cost from a provider-specific function', async () => {
    const {createLlmFetch} = await import('../index');
    const c = capture();
    const original = globalThis.fetch;
    globalThis.fetch = (async () => new Response(JSON.stringify({choices: [{message: {content: 'x'}}], usage: {cost: 0, is_byok: true, cost_details: {upstream_inference_cost: 0.02}}}), {headers: {'content-type': 'application/json'}})) as any;
    try {
        await createLlmFetch({cost: u => u.is_byok ? u.cost_details.upstream_inference_cost : u.cost})('https://llm.example.com', {method: 'POST', body: '{"model":"m"}'});
    } finally {
        globalThis.fetch = original;
    }
    assert.equal(c.records[0].costUsd, 0.02);
});

test('metrics: counters write their increase, gauges their value, histograms their buckets', async () => {
    const {metrics, flushMetrics, setMetricsWriter, setTracing} = await import('../index');
    setTracing(true);
    const lines: any[] = [];
    setMetricsWriter(l => lines.push(JSON.parse(l)));
    metrics.counter('orders', {outcome: 'paid'});
    metrics.counter('orders', {outcome: 'paid'}, 2);
    metrics.counter('orders', {outcome: 'failed', skip: undefined});
    metrics.gauge('connections', 4);
    metrics.buckets('dur', [1, 2]);
    metrics.observe('dur', 0.5);
    metrics.observe('dur', 1.5);
    metrics.observe('dur', 9);
    metrics.gaugeFn('queue', async () => 12, {kind: 'studio'});
    metrics.gaugeFn('broken', () => { throw new Error('x'); });
    await flushMetrics(new Date('2026-01-02T10:00:00Z'));
    const by = (n: string) => lines.filter(l => l.name === n);
    assert.ok(lines.every(l => l.$ === 'metric' && l.ts === '2026-01-02T10:00:00.000Z'));
    assert.deepEqual(by('orders').map(l => [l.labels.outcome, l.value]).sort(), [['failed', 1], ['paid', 3]]);
    assert.equal(by('connections')[0].value, 4);
    assert.deepEqual(by('dur')[0], {$: 'metric', ts: '2026-01-02T10:00:00.000Z', name: 'dur', kind: 'histogram', count: 3, sum: 11, min: 0.5, max: 9, bounds: [1, 2], counts: [1, 1, 1]});
    assert.deepEqual(by('queue')[0].labels, {kind: 'studio'});
    assert.equal(by('broken').length, 0);
    lines.length = 0;
    await flushMetrics(new Date('2026-01-02T10:01:00Z'));
    // The next minute: counters and histograms start at zero, gauges are written again.
    assert.deepEqual(lines.map(l => l.name).sort(), ['connections', 'queue']);
    setMetricsWriter(null);
});

test('metrics derived from trace records: models, tools, outbound calls, other calls, warn/error lines', async () => {
    const {deriveMetrics, emitTrace, flushMetrics, setMetricsWriter, setTracing} = await import('../index');
    setTracing(true);
    setTraceWriter(() => undefined);
    const lines: any[] = [];
    setMetricsWriter(l => lines.push(JSON.parse(l)));
    const off = deriveMetrics();
    try {
        emitTrace({type: 'llm', name: 'm1', durationMs: 1500, costUsd: 0.02, ok: true, data: {route: 'dialogue', usage: {prompt_tokens: 100, completion_tokens: 20}}});
        emitTrace({type: 'llm', name: 'm1', durationMs: 500, ok: false, data: {route: 'dialogue'}});
        emitTrace({type: 'tool', name: 'lookup', durationMs: 30, ok: true});
        emitTrace({type: 'http', name: 'POST api.example.com/v1/x', durationMs: 200, ok: false, data: {dir: 'out', status: 503}});
        emitTrace({type: 'http', name: 'GET /orders', data: {dir: 'in'}});
        emitTrace({type: 'storage', name: 'PutObject a/b.png', durationMs: 40, ok: true});
        emitTrace({type: 'log', name: 'Billing', level: 'error', data: {message: 'x'}});
        emitTrace({type: 'log', name: 'Billing', level: 'info', data: {message: 'y'}});
        emitTrace({type: 'note', name: 'boot'});
        await flushMetrics();
    } finally {
        off();
        setMetricsWriter(null);
        setTraceWriter(null);
    }
    const get = (name: string, labels: Record<string, string> = {}) =>
        lines.filter(l => l.name === name && Object.entries(labels).every(([k, v]) => l.labels?.[k] === v));
    assert.equal(get('llm_calls', {model: 'm1', outcome: 'ok'})[0].value, 1);
    assert.equal(get('llm_calls', {outcome: 'error'})[0].value, 1);
    assert.equal(get('llm_cost_usd', {route: 'dialogue'})[0].value, 0.02);
    assert.equal(get('llm_tokens', {direction: 'in'})[0].value, 100);
    assert.equal(get('llm_seconds', {route: 'dialogue'})[0].count, 2);
    assert.equal(get('tool_calls', {tool: 'lookup'})[0].value, 1);
    assert.equal(get('outbound_requests', {host: 'api.example.com', status: '5xx'})[0].value, 1);
    assert.equal(get('outbound_requests').length, 1); // inbound requests are not counted
    assert.equal(get('calls', {type: 'storage', outcome: 'ok'})[0].value, 1);
    assert.equal(get('log_lines', {level: 'error', context: 'Billing'})[0].value, 1);
    assert.equal(get('log_lines').length, 1);
});

test('metrics.probe times a function on a timer and counts failures', async () => {
    const {metrics, flushMetrics, setMetricsWriter, setTracing} = await import('../index');
    setTracing(true);
    const lines: any[] = [];
    setMetricsWriter(l => lines.push(JSON.parse(l)));
    let n = 0;
    const stop = metrics.probe('db', async () => {
        n++;
        if (n === 2) throw new Error('down');
        if (n === 3) return false;
    }, 5);
    await new Promise(r => setTimeout(r, 60));
    stop();
    const calls = n;
    await new Promise(r => setTimeout(r, 30));
    assert.equal(n, calls); // stopped
    await flushMetrics();
    setMetricsWriter(null);
    const seconds = lines.find(l => l.name === 'db_probe_seconds');
    assert.equal(lines.find(l => l.name === 'db_probe_failures').value, 1);
    assert.equal(seconds.count, calls - 1); // the skipped round is not timed
});
