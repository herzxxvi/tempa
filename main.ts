// Launcher for the forge.
//
// The forge itself is one static Go binary (sources under cmd/ and internal/, built by build.sh).
// It binds its own port, registers with the environment named by MAKEFUN_ENV_URL, serves until the
// environment says the trial is over and exits 0. This file starts that binary and passes its exit
// code on, so `node main.ts` behaves exactly like the binary.

import { spawn } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import { lookup } from 'node:dns/promises';
import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { isIP } from 'node:net';
import { constants, homedir, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const windows = process.platform === 'win32';

/**
 * The binary to run: the Linux build committed in bin/ next to this file.
 *
 * Development only: FORGE_BIN names another build to run instead, and a platform whose build is not
 * part of the checkout (bin/ holds only the committed Linux binary) uses the copy build.sh keeps in
 * the per-user cache directory.
 */
function locateBinary(): string {
  if (process.env.FORGE_BIN) return process.env.FORGE_BIN;
  const name = windows ? 'forge-windows-amd64.exe' : 'forge-linux-amd64';
  const bundled = join(dirname(fileURLToPath(import.meta.url)), 'bin', name);
  return existsSync(bundled) ? bundled : join(homedir(), '.cache', 'forge', name);
}

/**
 * MAKEFUN_ENV_URL with its host name replaced by the address the system resolver finds for it, or
 * undefined when there is nothing to resolve. The binary is statically linked and resolves names
 * with its own resolver; given the address as well (it tries this URL first and the original one
 * after it), a name that only the system's resolver configuration can resolve cannot keep it from
 * reaching the environment.
 */
async function resolveEnvUrl(): Promise<string | undefined> {
  try {
    const url = new URL(process.env.MAKEFUN_ENV_URL ?? '');
    if (url.protocol !== 'http:' || isIP(url.hostname.replace(/^\[|\]$/g, '')) !== 0) return undefined;
    const giveUp = new Promise<never>((_, reject) => setTimeout(reject, 2000, new Error('lookup timed out')).unref());
    const found = await Promise.race([lookup(url.hostname, { all: true }), giveUp]);
    // An IPv4 address when there is one: it reaches a server bound to either address family.
    const pick = found.find((a) => a.family === 4) ?? found[0];
    if (pick === undefined) return undefined;
    url.hostname = pick.family === 6 ? `[${pick.address}]` : pick.address;
    return url.href;
  } catch {
    return undefined;
  }
}

// Runtime settings for the binary; anything already set in the environment wins. The binary applies
// the first three and FORGE_IO itself as well, so those only document them where an operator looks
// first.
//
// GODEBUG=schedtrace=<a very long interval> is not about tracing. Go's monitor thread goes into a
// deep sleep whenever the program is idle, which a server with one worker is between any two
// requests, and the next system call then has to wake it with a futex call, only for it to go back
// to sleep moments later. That is two or three extra system calls and a thread wake-up per request,
// which is costly where system calls are (a sandboxed kernel). With schedtrace set, the monitor
// stays on its ordinary 10 ms tick and nobody has to wake it. The interval is 23 days, so no trace
// line is ever printed. The runtime reads this before the program's own code runs, so it can only
// be set here.
//
// FORGE_IO=epoll (Linux; FORGE_IO=netpoll switches back): the binary's own epoll loop instead of
// Go's netpoller. In a gVisor lab it measured +1.39 ± 0.21 speed points over netpoll across 150
// paired runs, with no failure in 185 runs and 43k -> 30.5k system calls per pass. The Windows
// build has no such loop and keeps the netpoller, so nothing is set there.
//
// FORGE_IO=hybrid (Linux, an experiment): the epoll loop, lending a connection that is used alone,
// one request after the other, to a goroutine in a blocking read (two system calls per exchange
// instead of three) until it goes quiet. That needs spare Ps, so the binary runs it with
// GOMAXPROCS=4 whatever is set here.
const env: NodeJS.ProcessEnv = {
  GOMAXPROCS: '1',
  GOGC: 'off',
  GOMEMLIMIT: '256MiB',
  GODEBUG: 'schedtrace=2000000000',
  ...(windows ? {} : { FORGE_IO: 'epoll' }),
  ...process.env,
};

// Where to put an executable copy of the binary when the checkout's own file may not be executed
// (a lost exec bit on a read-only mount, a mount that forbids execution, a policy that refuses it).
const fallbackDirs = windows ? [tmpdir()] : [tmpdir(), '/dev/shm'];

const binary = locateBinary();
let child: ChildProcess | undefined;

function fail(message: string): never {
  process.stderr.write(`[launcher] ${message}\n`);
  process.exit(1);
}

/** Start the binary at `path`; `nextFallback` is the first fallback directory not tried yet. */
function start(path: string, nextFallback: number): void {
  const proc = spawn(path, process.argv.slice(2), { stdio: 'inherit', env, windowsHide: true });
  child = proc;
  proc.once('error', (err: Error & { code?: string }) => {
    if (child !== proc) return;
    child = undefined;
    if (err.code === 'EACCES' || err.code === 'EPERM') {
      for (let i = nextFallback; i < fallbackDirs.length; i += 1) {
        const copy = join(fallbackDirs[i], `forge-${process.pid}${windows ? '.exe' : ''}`);
        try {
          copyFileSync(binary, copy);
          chmodSync(copy, 0o755);
        } catch {
          continue; // this directory cannot take the copy: try the next one
        }
        start(copy, i + 1);
        return;
      }
    }
    fail(`cannot start ${path}: ${err.message}`);
  });
  proc.once('exit', (code, signal) => {
    if (child !== proc) return;
    // The binary's exit code is ours. Killed by a signal: the shell convention 128 + signal number.
    process.exit(code ?? 128 + (signal ? constants.signals[signal] ?? 0 : 0));
  });
}

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    if (child === undefined || !child.kill(signal)) process.exit(128 + constants.signals[signal]);
  });
}

const resolved = await resolveEnvUrl();
if (resolved !== undefined) env.FORGE_ENV_URL = resolved;
start(binary, 0);
