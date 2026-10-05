/**
 * TASK-3415 — the world for the real-install e2e leg.
 *
 * Installed apps need an https OAuth issuer and an https app origin the pad
 * server trusts, which the shared e2e server (plain http, apps OFF by design)
 * does not have. This builds a second, private world:
 *
 *   - a per-run CA and a 127.0.0.1 leaf certificate (openssl);
 *   - pad itself, from the `-tags e2etest` build (PAD_E2E_BINARY), on plain
 *     http on its own port, with PAD_URL naming the https FRONT below and
 *     PAD_E2E_APP_CA naming the CA (the test-build-only door,
 *     cmd/pad/e2e_app_ca.go);
 *   - the front: a TLS terminator forwarding to pad with Host preserved, the
 *     way a real deployment sits behind one. Everything, the browser and the
 *     app alike, reaches pad through it; nothing calls pad's http port;
 *   - the app: an https server on the same CA that serves an apps/1 manifest
 *     and records the webhooks it receives.
 *
 * The app's own HTTP calls go through `appCall`, which trusts ONLY the test
 * CA, as a real app would trust a real chain.
 */

import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export interface Certs {
	dir: string;
	caPath: string;
	ca: Buffer;
	key: Buffer;
	cert: Buffer;
}

/** True when this machine can build the world (openssl on PATH). */
export function opensslAvailable(): boolean {
	try {
		execFileSync('openssl', ['version'], { stdio: 'ignore' });
		return true;
	} catch {
		return false;
	}
}

/** A CA, and a leaf for 127.0.0.1 it signed, in a fresh temp dir. */
export function makeCerts(): Certs {
	const dir = mkdtempSync(join(tmpdir(), 'pad-e2e-tls-'));
	const p = (f: string) => join(dir, f);
	const run = (...args: string[]) => execFileSync('openssl', args, { stdio: 'pipe' });
	run('req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=pad-e2e-ca',
		'-keyout', p('ca.key'), '-out', p('ca.pem'));
	run('req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=127.0.0.1', '-keyout', p('leaf.key'), '-out', p('leaf.csr'));
	writeFileSync(p('leaf.ext'), 'subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n');
	run('x509', '-req', '-in', p('leaf.csr'), '-CA', p('ca.pem'), '-CAkey', p('ca.key'), '-CAcreateserial',
		'-days', '2', '-extfile', p('leaf.ext'), '-out', p('leaf.pem'));
	return {
		dir,
		caPath: p('ca.pem'),
		ca: readFileSync(p('ca.pem')),
		key: readFileSync(p('leaf.key')),
		cert: readFileSync(p('leaf.pem'))
	};
}

export async function freePort(): Promise<number> {
	return new Promise((resolve, reject) => {
		const s = net.createServer();
		s.listen(0, '127.0.0.1', () => {
			const port = (s.address() as net.AddressInfo).port;
			s.close(() => resolve(port));
		});
		s.on('error', reject);
	});
}

/** The TLS front: https on frontPort, forwarding to pad's http port with Host kept. */
export async function startFront(certs: Certs, frontPort: number, padPort: number): Promise<https.Server> {
	const server = https.createServer({ key: certs.key, cert: certs.cert }, (req, res) => {
		const up = http.request(
			{ host: '127.0.0.1', port: padPort, method: req.method, path: req.url, headers: req.headers },
			(r) => {
				res.writeHead(r.statusCode ?? 502, r.headers);
				r.pipe(res);
			}
		);
		up.on('error', () => {
			if (!res.headersSent) res.writeHead(502);
			res.end();
		});
		req.pipe(up);
	});
	// WebSocket upgrades (the collab socket) pass through as raw bytes.
	server.on('upgrade', (req, socket, head) => {
		const up = net.connect(padPort, '127.0.0.1', () => {
			const lines = [`${req.method} ${req.url} HTTP/1.1`];
			for (let i = 0; i < req.rawHeaders.length; i += 2) lines.push(`${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}`);
			up.write(lines.join('\r\n') + '\r\n\r\n');
			if (head.length) up.write(head);
			up.pipe(socket);
			socket.pipe(up);
		});
		up.on('error', () => socket.destroy());
		socket.on('error', () => up.destroy());
	});
	await new Promise<void>((resolve) => server.listen(frontPort, '127.0.0.1', resolve));
	return server;
}

export interface RecordedHook {
	headers: http.IncomingHttpHeaders;
	raw: Buffer;
	body: Record<string, unknown>;
}

export interface AppServer {
	server: https.Server;
	origin: string;
	hooks: RecordedHook[];
}

/** The app: an https origin serving its manifest and recording webhooks. */
export async function startApp(certs: Certs, port: number, manifest: (origin: string) => unknown): Promise<AppServer> {
	const origin = `https://127.0.0.1:${port}`;
	const hooks: RecordedHook[] = [];
	const server = https.createServer({ key: certs.key, cert: certs.cert }, (req, res) => {
		if (req.method === 'GET' && req.url === '/.well-known/pad-app.json') {
			res.writeHead(200, { 'Content-Type': 'application/json' });
			res.end(JSON.stringify(manifest(origin)));
			return;
		}
		if (req.method === 'POST' && req.url === '/hooks') {
			const chunks: Buffer[] = [];
			req.on('data', (c) => chunks.push(c));
			req.on('end', () => {
				const raw = Buffer.concat(chunks);
				let body: Record<string, unknown> = {};
				try {
					body = JSON.parse(raw.toString('utf8'));
				} catch {
					// recorded as received
				}
				hooks.push({ headers: req.headers, raw, body });
				res.writeHead(204);
				res.end();
			});
			return;
		}
		res.writeHead(404);
		res.end();
	});
	await new Promise<void>((resolve) => server.listen(port, '127.0.0.1', resolve));
	return { server, origin, hooks };
}

/** Pad from the e2etest build, on plain http, addressed as https through the front. */
export function startPad(opts: {
	binary: string;
	padPort: number;
	frontOrigin: string;
	caPath: string;
}): { child: ChildProcess; dataDir: string } {
	const dataDir = mkdtempSync(join(tmpdir(), 'pad-e2e-real-install-'));
	const child = spawn(opts.binary, ['server', 'start'], {
		stdio: ['ignore', 'inherit', 'inherit'],
		env: {
			...process.env,
			HOME: dataDir,
			PAD_HOST: '127.0.0.1',
			PAD_PORT: String(opts.padPort),
			PAD_DATA_DIR: dataDir,
			PAD_URL: opts.frontOrigin,
			PAD_E2E_APP_CA: opts.caPath,
			PAD_OUTBOX_DRAIN_INTERVAL: '200ms',
			PAD_DISABLE_RATE_LIMITS: '1',
			PAD_LOG_LEVEL: 'warn'
		}
	});
	return { child, dataDir };
}

export interface AppResponse {
	status: number;
	headers: http.IncomingHttpHeaders;
	text: string;
	json: () => any; // eslint-disable-line @typescript-eslint/no-explicit-any
}

/** An HTTPS call that trusts ONLY the test CA, as the app makes it. */
export function appCall(
	ca: Buffer,
	method: string,
	url: string,
	opts: { headers?: Record<string, string>; body?: string | Buffer } = {}
): Promise<AppResponse> {
	return new Promise((resolve, reject) => {
		const u = new URL(url);
		if (u.protocol !== 'https:') {
			reject(new Error(`the app only speaks https; refusing ${url}`));
			return;
		}
		const headers: Record<string, string> = { ...(opts.headers ?? {}) };
		if (opts.body !== undefined) headers['Content-Length'] = String(Buffer.byteLength(opts.body));
		const req = https.request(
			{ host: u.hostname, port: u.port, path: u.pathname + u.search, method, headers, ca, agent: false },
			(res) => {
				const chunks: Buffer[] = [];
				res.on('data', (c) => chunks.push(c));
				res.on('end', () => {
					const text = Buffer.concat(chunks).toString('utf8');
					resolve({ status: res.statusCode ?? 0, headers: res.headers, text, json: () => JSON.parse(text) });
				});
			}
		);
		req.on('error', reject);
		if (opts.body !== undefined) req.write(opts.body);
		req.end();
	});
}

/** Waits until fn returns a truthy value, or fails after timeoutMs. */
export async function waitFor<T>(
	what: string,
	fn: () => T | null | undefined | false | Promise<T | null | undefined | false>,
	timeoutMs = 20_000
): Promise<T> {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		const v = await fn();
		if (v) return v;
		if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
		await new Promise((r) => setTimeout(r, 100));
	}
}

/** Verifies a delivery's X-Pad-Signature-256 against the redeemed secret. */
export function signatureVerifies(secret: string, hook: RecordedHook): boolean {
	const sig = String(hook.headers['x-pad-signature-256'] ?? '');
	const m = /^t=(\d+),v1=([0-9a-f]+)$/.exec(sig);
	if (!m) return false;
	const mac = createHmac('sha256', secret);
	mac.update(m[1] + '.');
	mac.update(hook.raw);
	return mac.digest('hex') === m[2];
}

export function cleanupDirs(...dirs: string[]) {
	for (const d of dirs) rmSync(d, { recursive: true, force: true });
}
