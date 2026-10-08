import { describe, it, expect, vi } from 'vitest';
import { IMPORT_FINISHING_MS, IMPORT_STALL_MS, ImportTransportError, uploadImportBundle } from './importUpload';

// BUG-3475: the upload's two bounds and its progress, against a fake XHR and
// a hand-driven timer, so "no progress for 60s" is tested without waiting.

class FakeXHR {
	upload: { onprogress: ((e: ProgressEvent) => void) | null; onload: (() => void) | null } = { onprogress: null, onload: null };
	onload: (() => void) | null = null;
	onerror: (() => void) | null = null;
	ontimeout: (() => void) | null = null;
	onabort: (() => void) | null = null;
	withCredentials = false;
	status = 0;
	responseText = '';
	headers: Record<string, string> = {};
	sent: unknown = undefined;
	aborted = false;
	open = vi.fn();
	setRequestHeader = vi.fn((k: string, v: string) => {
		this.headers[k] = v;
	});
	send(body: unknown) {
		this.sent = body;
	}
	abort() {
		this.aborted = true;
		this.onabort?.();
	}
	getResponseHeader(name: string) {
		return name === 'X-Test' ? 'yes' : null;
	}
	progress(loaded: number, total: number) {
		this.upload.onprogress?.({ loaded, total, lengthComputable: true } as ProgressEvent);
	}
}

function harness() {
	const xhr = new FakeXHR();
	const timers: { fn: () => void; ms: number; id: number; live: boolean }[] = [];
	let next = 1;
	const setTimer = (fn: () => void, ms: number) => {
		const t = { fn, ms, id: next++, live: true };
		timers.push(t);
		return t.id;
	};
	const clearTimer = (id: unknown) => {
		const t = timers.find((x) => x.id === id);
		if (t) t.live = false;
	};
	/** Fire the one live timer, asserting its length. */
	const fire = (ms: number) => {
		const live = timers.filter((t) => t.live);
		expect(live.map((t) => t.ms)).toEqual([ms]);
		live[0].live = false;
		live[0].fn();
	};
	const live = () => timers.filter((t) => t.live).map((t) => t.ms);
	return { xhr, setTimer, clearTimer, fire, live };
}

function start(h: ReturnType<typeof harness>, extra: Partial<Parameters<typeof uploadImportBundle>[0]> = {}) {
	return uploadImportBundle({
		url: '/api/v1/workspaces/import?import_key=k',
		headers: { 'Content-Type': 'application/gzip', 'X-CSRF-Token': 't' },
		body: new Blob([new Uint8Array(100)]),
		xhr: () => h.xhr as unknown as XMLHttpRequest,
		setTimer: h.setTimer,
		clearTimer: h.clearTimer,
		...extra
	});
}

describe('uploadImportBundle', () => {
	it('sends the body with its headers and credentials, and resolves with the response', async () => {
		const h = harness();
		const p = start(h);
		expect(h.xhr.open).toHaveBeenCalledWith('POST', '/api/v1/workspaces/import?import_key=k');
		expect(h.xhr.headers['X-CSRF-Token']).toBe('t');
		expect(h.xhr.withCredentials).toBe(true);
		h.xhr.status = 201;
		h.xhr.responseText = '{"slug":"x"}';
		h.xhr.onload?.();
		const r = await p;
		expect(r.status).toBe(201);
		expect(r.body).toBe('{"slug":"x"}');
		expect(r.header('X-Test')).toBe('yes');
		expect(h.live()).toEqual([]);
	});

	it('resolves with a 4xx too: only a missing response is a transport failure', async () => {
		const h = harness();
		const p = start(h);
		h.xhr.status = 400;
		h.xhr.responseText = '{"error":{"code":"bad_bundle","message":"x"}}';
		h.xhr.onload?.();
		expect((await p).status).toBe(400);
	});

	it('reports progress and gives up after IMPORT_STALL_MS without any', async () => {
		const h = harness();
		const seen: number[] = [];
		const p = start(h, { onProgress: (sent) => seen.push(sent) });
		h.xhr.progress(40, 100);
		h.xhr.progress(60, 100);
		expect(seen).toEqual([0, 40, 60]);
		// Each progress event re-armed the window: one live timer, the stall one.
		h.fire(IMPORT_STALL_MS);
		await expect(p).rejects.toEqual(new ImportTransportError('stalled'));
		expect(h.xhr.aborted).toBe(true);
	});

	it('a path dead from the first byte is a stall too', async () => {
		const h = harness();
		const p = start(h);
		h.fire(IMPORT_STALL_MS);
		await expect(p).rejects.toMatchObject({ kind: 'stalled' });
	});

	it('after the last byte, waits IMPORT_FINISHING_MS for a response and then gives up', async () => {
		const h = harness();
		const uploaded = vi.fn();
		const p = start(h, { onUploaded: uploaded });
		h.xhr.progress(100, 100);
		h.xhr.upload.onload?.();
		expect(uploaded).toHaveBeenCalledOnce();
		h.fire(IMPORT_FINISHING_MS);
		await expect(p).rejects.toMatchObject({ kind: 'no_response' });
	});

	it('a network error and a cancel are reported as such, once', async () => {
		const h1 = harness();
		const p1 = start(h1);
		h1.xhr.onerror?.();
		await expect(p1).rejects.toMatchObject({ kind: 'network' });

		const h2 = harness();
		const ctl = new AbortController();
		const p2 = start(h2, { signal: ctl.signal });
		ctl.abort();
		await expect(p2).rejects.toMatchObject({ kind: 'aborted' });
		expect(h2.xhr.aborted).toBe(true);
		expect(h2.live()).toEqual([]);
	});

	it('a callback that throws does not wedge the upload', async () => {
		const h = harness();
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		const p = start(h, {
			onProgress: () => {
				throw new Error('progress bug');
			},
			onUploaded: () => {
				throw new Error('uploaded bug');
			}
		});
		h.xhr.progress(100, 100);
		h.xhr.upload.onload?.();
		h.xhr.status = 201;
		h.xhr.responseText = '{}';
		h.xhr.onload?.();
		expect((await p).status).toBe(201);
		expect(h.live()).toEqual([]);
		expect(err).toHaveBeenCalled();
		err.mockRestore();
	});

	it('an already-aborted signal sends nothing', async () => {
		const h = harness();
		const ctl = new AbortController();
		ctl.abort();
		await expect(start(h, { signal: ctl.signal })).rejects.toMatchObject({ kind: 'aborted' });
		expect(h.xhr.sent).toBeUndefined();
	});
});
