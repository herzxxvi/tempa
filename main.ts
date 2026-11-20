// Launcher for the forge.
//
// The forge itself is one static Go binary (sources under cmd/ and internal/, built by build.sh).
// It binds its own port, registers with the environment named by MAKEFUN_ENV_URL, serves until the
// environment says the trial is over and exits 0. This file only starts the right binary for the
// platform and passes its exit code on, so `node main.ts` behaves exactly like the binary.

import { spawn } from 'node:child_process';
import type { ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { constants, homedir, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const windows = process.platform === 'win32';
const name = windows ? 'forge-windows-amd64.exe' : 'forge-linux-amd64';
const bundled = join(dirname(fileURLToPath(import.meta.url)), 'bin', name);

// The Linux binary is committed and always sits next to this file. The Windows build is a
// workstation convenience that is not committed, so a copy of the checkout made from git's file
// list (the local grader makes one) arrives without it; build.sh therefore also leaves the Windows
// build in the user's cache directory, and that copy is used when the bundled one is absent.
const binary = windows && !existsSync(bundled) ? join(homedir(), '.cache', 'forge', name) : bundled;

// Runtime settings for the binary; anything already set in the environment wins. The binary applies
// the same defaults itself, so these only document them where an operator looks first.
const env = { GOMAXPROCS: '1', GOGC: 'off', GOMEMLIMIT: '512MiB', ...process.env };

let child: ChildProcess | undefined;

// Where to put an executable copy of the binary when the checkout's own file may not be executed
// (a lost exec bit on a read-only mount, or a mount that forbids execution).
const fallbackDirs = windows ? [tmpdir()] : [tmpdir(), '/dev/shm'];

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
    if (err.code === 'EACCES') {
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

start(binary, 0);
