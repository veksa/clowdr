// CPU profiles on demand, without an endpoint: `clowctl profile <service>` writes the length to
// PROFILE_DIR/request and sends SIGUSR2 to every process that left a pid file here; each one
// profiles itself with the built-in inspector and writes PROFILE_DIR/<pid>.cpuprofile, which
// clowctl downloads. Open it in Chrome DevTools → Performance.

import {mkdirSync, readFileSync, renameSync, rmSync, writeFileSync} from 'node:fs';
import {join} from 'node:path';

export const PROFILE_DIR = '/tmp/clowdr-node';
const DEFAULT_SECONDS = 30;
const MAX_SECONDS = 300;

let installed = false;
let busy = false;

/**
 * Register this process for `clowctl profile` (installTracing does it where clowdr runs the
 * service). Idempotent; a process that cannot write PROFILE_DIR is simply not profilable.
 */
export function installProfiler(dir: string = PROFILE_DIR): void {
    if (installed) return;
    const pidFile = join(dir, `${process.pid}.pid`);
    try {
        mkdirSync(dir, {recursive: true});
        writeFileSync(pidFile, String(process.pid));
    } catch {
        return;
    }
    installed = true;
    process.on('SIGUSR2', () => void profileOnce(dir));
    process.once('exit', () => {
        try {
            rmSync(pidFile, {force: true});
        } catch {
            // the container is going away anyway
        }
    });
}

function requestedSeconds(dir: string): number {
    try {
        const n = Number(readFileSync(join(dir, 'request'), 'utf8').trim());
        if (Number.isFinite(n) && n > 0) return Math.min(n, MAX_SECONDS);
    } catch {
        // no request file: the default
    }
    return DEFAULT_SECONDS;
}

/** Profile this process for the requested seconds and write <pid>.cpuprofile. One at a time. */
export async function profileOnce(dir: string = PROFILE_DIR, seconds?: number): Promise<string | undefined> {
    if (busy) return undefined;
    busy = true;
    const {Session} = await import('node:inspector');
    const session = new Session();
    const post = (method: string, params?: object) => new Promise<any>((resolve, reject) =>
        session.post(method, params ?? {}, (err, result) => (err ? reject(err) : resolve(result))));
    const out = join(dir, `${process.pid}.cpuprofile`);
    try {
        session.connect();
        await post('Profiler.enable');
        await post('Profiler.start');
        const ms = (seconds ?? requestedSeconds(dir)) * 1000;
        await new Promise(resolve => setTimeout(resolve, ms));
        const {profile} = await post('Profiler.stop');
        // Written whole, then renamed: clowctl never reads half a profile.
        writeFileSync(`${out}.part`, JSON.stringify(profile));
        renameSync(`${out}.part`, out);
        return out;
    } catch {
        return undefined;
    } finally {
        try {
            session.disconnect();
        } catch {
            // already gone
        }
        busy = false;
    }
}
