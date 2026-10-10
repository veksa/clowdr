// clowdr-node: traces for services deployed with clowdr. Records are JSON lines on stdout that
// clowdrd keeps in the service's trace stream; read them with `clowctl trace`. See README.md.

import {traceFetch} from './fetch';
import {traceAxios} from './integrations';
import {emitTrace} from './record';

export * from './context';
export * from './record';
export * from './sanitize';
export {traceFetch, untraceFetch, llmFetch, baseFetch, setBaseFetch, hostAndPath, stripQuery} from './fetch';
export {traceAxios, traceS3, tracePrismaAdapter, statementKind} from './integrations';
export {httpTraceMiddleware, defaultQuiet, type HttpTraceOptions} from './http';

/** `llmFetch` under the name existing services use. */
export {llmFetch as tracedFetch} from './fetch';

export interface InstallOptions {
    /** axios instances to record (usually the default export of `axios`). */
    axios?: any[];
    /** Extra fields of the boot record (environment, version). */
    boot?: Record<string, any>;
}

let installed = false;

/**
 * Patch the global fetch and the given axios instances, and write a `server.boot` note. Call
 * once at start, before the HTTP server listens. Idempotent.
 */
export function installTracing(options: InstallOptions = {}): void {
    traceFetch();
    for (const instance of options.axios ?? []) traceAxios(instance);
    if (installed) return;
    installed = true;
    emitTrace({type: 'note', name: 'server.boot', data: {pid: process.pid, ...options.boot}});
    process.once('beforeExit', () => emitTrace({type: 'note', name: 'server.shutdown', data: {pid: process.pid}}));
}
