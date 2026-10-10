// Metrics derived from trace records: every service that traces gets them without a line of its own.
// Labels are kept to closed sets — a model, a route, a tool, a host, a record type — never an id,
// a URL path or a message.

import {metrics} from './metrics';
import {onTraceRecord, RecordInput} from './record';

const HANDLED = new Set(['llm', 'tool', 'http', 'log', 'note', 'job', 'event', 'analytics', 'socket', 'db']);

function outcome(r: RecordInput): string {
    return r.ok === false ? 'error' : 'ok';
}

function seconds(r: RecordInput): number | undefined {
    return typeof r.durationMs === 'number' ? r.durationMs / 1000 : undefined;
}

/** `POST api.example.com/v1/x` → `api.example.com`. */
function hostOf(name: string): string {
    const target = name.split(' ')[1] ?? name;
    return target.split('/')[0] || 'unknown';
}

function statusClass(status: unknown): string {
    return typeof status === 'number' ? `${Math.floor(status / 100)}xx` : 'error';
}

/**
 * Turn trace records into metrics (called by installTracing unless `metrics: false`):
 *
 * - llm_calls{model,route,outcome}, llm_cost_usd{model,route}, llm_tokens{model,direction}, llm_seconds{route}
 * - tool_calls{tool,outcome}, tool_seconds{tool}
 * - outbound_requests{host,status}, outbound_seconds{host} — outgoing HTTP (fetch, axios)
 * - calls{type,outcome}, call_seconds{type} — any other timed call (storage, telegram, mail, payment…)
 * - log_lines{level,context} — warn and error lines
 *
 * Returns the unsubscribe.
 */
export function deriveMetrics(): () => void {
    return onTraceRecord(r => {
        const d = r.data ?? {};
        const s = seconds(r);
        switch (r.type) {
            case 'llm': {
                const route = typeof d.route === 'string' ? d.route : 'unknown';
                metrics.counter('llm_calls', {model: r.name, route, outcome: outcome(r)});
                if (typeof r.costUsd === 'number' && r.costUsd > 0) metrics.counter('llm_cost_usd', {model: r.name, route}, r.costUsd);
                const usage = d.usage ?? {};
                if (typeof usage.prompt_tokens === 'number') metrics.counter('llm_tokens', {model: r.name, direction: 'in'}, usage.prompt_tokens);
                if (typeof usage.completion_tokens === 'number') metrics.counter('llm_tokens', {model: r.name, direction: 'out'}, usage.completion_tokens);
                if (s !== undefined) metrics.observe('llm_seconds', s, {route});
                return;
            }
            case 'tool':
                metrics.counter('tool_calls', {tool: r.name, outcome: outcome(r)});
                if (s !== undefined) metrics.observe('tool_seconds', s, {tool: r.name});
                return;
            case 'http':
                if (d.dir !== 'out') return; // inbound requests: the nginx access log has them
                metrics.counter('outbound_requests', {host: hostOf(r.name), status: statusClass(d.status)});
                if (s !== undefined) metrics.observe('outbound_seconds', s, {host: hostOf(r.name)});
                return;
            case 'log':
                if (r.level === 'warn' || r.level === 'error') metrics.counter('log_lines', {level: r.level, context: r.name});
                return;
        }
        if (HANDLED.has(r.type) || r.ok === undefined) return;
        metrics.counter('calls', {type: r.type, outcome: outcome(r)});
        if (s !== undefined) metrics.observe('call_seconds', s, {type: r.type});
    });
}
