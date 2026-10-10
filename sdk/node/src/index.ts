// clowdr-node: traces for services deployed with clowdr. Records are JSON lines on stdout that
// clowdrd keeps in the service's trace stream; read them with `clowctl trace`. See README.md.

import {traceFetch} from './fetch';
import {traceAxios} from './integrations';
import {emitTrace} from './record';
import {deriveMetrics} from './derived';
import {installProfiler} from './profile';
import {isTracing} from './record';

export * from './context';
export * from './record';
export * from './sanitize';
export {traceFetch, untraceFetch, llmFetch, createLlmFetch, callerFrame, traceTools, baseFetch, setBaseFetch, hostAndPath, stripQuery, type LlmFetchOptions} from './fetch';
export {traceAxios, traceS3, tracePrismaAdapter, tracePrismaQuery, tracePgPool, traceRedis, traceCall, traceMethods, statementKind, type TracedCall} from './integrations';
export {httpTraceMiddleware, defaultQuiet, TRACE_ID_HEADER, type HttpTraceOptions} from './http';
export {metrics, flushMetrics, setMetricsWriter, DEFAULT_BOUNDS, type Labels} from './metrics';
export {deriveMetrics} from './derived';
export {installProfiler, profileOnce, PROFILE_DIR} from './profile';

/** `llmFetch` under the name existing services use. */
export {llmFetch as tracedFetch} from './fetch';

export interface InstallOptions {
    /**
     * Patch the global fetch (default true). Off for a service that wraps its outgoing calls
     * itself (traceCall), so they are not recorded twice.
     */
    fetch?: boolean;
    /** axios instances to record (usually the default export of `axios`). */
    axios?: any[];
    /** Metrics derived from trace records (llm_*, tool_*, outbound_*, calls, log_lines). Default true. */
    metrics?: boolean;
    /** Answer `clowctl profile` (SIGUSR2 → a CPU profile). Default true where clowdr runs the service. */
    profile?: boolean;
    /** Extra fields of the boot record (environment, version). */
    boot?: Record<string, any>;
}

let installed = false;

/**
 * Patch the global fetch and the given axios instances, and write a `server.boot` note. Call
 * once at start, before the HTTP server listens. Idempotent.
 */
export function installTracing(options: InstallOptions = {}): void {
    if (options.fetch !== false) traceFetch();
    for (const instance of options.axios ?? []) traceAxios(instance);
    if (installed) return;
    installed = true;
    if (options.metrics !== false) deriveMetrics();
    // Only under clowdr: locally SIGUSR2 belongs to tools like nodemon.
    if (options.profile !== false && isTracing()) installProfiler();
    emitTrace({type: 'note', name: 'server.boot', data: {pid: process.pid, ...options.boot}});
    process.once('beforeExit', () => emitTrace({type: 'note', name: 'server.shutdown', data: {pid: process.pid}}));
}
