#!/usr/bin/env node
// Cross-platform launcher used by Playwright's `webServer.command`.
// Wipes the data dir (fresh migrations every run) and then exec's the
// pad binary. POSIX-only alternatives like `rm -rf && mkdir -p && pad ...`
// break Windows contributors (cmd.exe / PowerShell don't understand
// those commands); Node's fs APIs work uniformly on every platform.

import { spawn } from 'node:child_process';
import { mkdirSync, rmSync } from 'node:fs';

const dataDir = process.env.PAD_DATA_DIR;
if (!dataDir) {
	console.error('run-pad: PAD_DATA_DIR is required');
	process.exit(2);
}
const binary = process.env.PAD_BINARY;
if (!binary) {
	console.error('run-pad: PAD_BINARY is required');
	process.exit(2);
}

rmSync(dataDir, { recursive: true, force: true });
mkdirSync(dataDir, { recursive: true });

// Forward stdio so Playwright can wait for the server to bind the port,
// and so failures surface in the test report. Pass the same env through
// so PAD_HOST, PAD_PORT, PAD_DATA_DIR, PAD_LOG_LEVEL all reach the binary.
const child = spawn(binary, ['server', 'start'], { stdio: 'inherit' });

const forward = (sig) => () => {
	if (!child.killed) child.kill(sig);
};
process.on('SIGINT', forward('SIGINT'));
process.on('SIGTERM', forward('SIGTERM'));

child.on('exit', (code, signal) => {
	if (signal) process.kill(process.pid, signal);
	else process.exit(code ?? 0);
});
