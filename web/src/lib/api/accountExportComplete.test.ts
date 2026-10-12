import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, accountExportIsComplete } from './client';

// BUG-3626: an account export is saved only when its body carries the
// server's "complete": true trailer. The server aborts a timed-out or failed
// export mid-body; whatever arrives must not be saved as the user's account.

function stubExport(text: () => Promise<string>) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => ({
			status: 200,
			ok: true,
			headers: {
				get: (n: string) =>
					n.toLowerCase() === 'content-disposition'
						? 'attachment; filename="pad-account-export.json"'
						: null
			},
			text
		}))
	);
}

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('accountExportIsComplete', () => {
	it('accepts a body with the complete trailer', () => {
		expect(accountExportIsComplete('{"format":"pad-account-export","workspaces":[],"complete":true}')).toBe(true);
	});
	it('refuses a valid body without it (the pre-fix truncated shape)', () => {
		expect(accountExportIsComplete('{"user":{},"workspaces":[]}')).toBe(false);
	});
	it('refuses complete set to anything but true', () => {
		expect(accountExportIsComplete('{"complete":"true"}')).toBe(false);
		expect(accountExportIsComplete('{"complete":false}')).toBe(false);
	});
	it('refuses a cut-off body and non-JSON', () => {
		expect(accountExportIsComplete('{"user":{},"workspaces":[{"id":')).toBe(false);
		expect(accountExportIsComplete('null')).toBe(false);
		expect(accountExportIsComplete('')).toBe(false);
	});
});

describe('api.auth.exportAccountData (BUG-3626)', () => {
	it('returns a complete export with its filename', async () => {
		stubExport(async () => '{"workspaces":[],"complete":true}');
		await expect(api.auth.exportAccountData()).resolves.toEqual({
			filename: 'pad-account-export.json',
			text: '{"workspaces":[],"complete":true}'
		});
	});
	it('rejects a 200 whose body lacks the trailer, so nothing is saved', async () => {
		stubExport(async () => '{"user":{},"workspaces":[]}');
		await expect(api.auth.exportAccountData()).rejects.toMatchObject({ code: 'export_incomplete' });
	});
	it('rejects when the server cut the body off', async () => {
		stubExport(async () => {
			throw new TypeError('network error');
		});
		await expect(api.auth.exportAccountData()).rejects.toThrow();
	});
});
