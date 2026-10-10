import {test} from 'node:test';
import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {
    emitTrace, httpTraceMiddleware, llmFetch, runInTrace, setChannel, setTraceUser, setTraceWriter,
    traceFetch, traceJob, untraceFetch, channelEnabled, traceS3,
} from '../index';

function capture(): {records: any[]; lines: string[]} {
    const out = {records: [] as any[], lines: [] as string[]};
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
    const request = (method: string, url: string, status: number, body?: string) => {
        const res: any = Object.assign(new EventEmitter(), {statusCode: 200, send(b: any) { return b; }});
        mw({method, originalUrl: url, query: {t: 'x'}, body: {a: 1}}, res, () => {
            emitTrace({type: 'note', name: 'inside'});
            res.statusCode = status;
            if (body) res.send(body);
        });
        res.emit('finish');
    };
    request('POST', '/orders?x=1', 400, '{"message":"bad"}');
    request('GET', '/health', 200);
    request('GET', '/health', 503);
    const [inbound, inside, out] = c.records;
    assert.equal(inbound.name, 'POST /orders');
    assert.equal(inbound.source, 'http:POST /orders');
    assert.equal(inside.traceId, inbound.traceId);
    assert.equal(out.name, 'POST /orders ←');
    assert.deepEqual(out.data.error, {message: 'bad'});
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
