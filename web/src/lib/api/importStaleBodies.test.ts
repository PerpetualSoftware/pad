import { describe, it, expect, vi, afterEach } from 'vitest';
import { api } from './client';

// BUG-3032: the import's stale-body count arrives as a RESPONSE HEADER, so it is
// returned alongside the workspace rather than inside it.
//
// These cases exist because a response header is middlebox- and
// attacker-influenced input, and the two transports must suppress and display
// exactly the same set of values. Codex round 2 caught the web client accepting
// what the CLI rejects (`Number()` trims and takes decimals); round 3 caught the
// rest of the gap — exponent notation, hex, and integers past 2^53, all of which
// Go's strconv.Atoi refuses and `Number()` happily converts or rounds.
function mockImportOnce(headerValue: string | null) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => ({
			status: 201,
			ok: true,
			headers: { get: (k: string) => (k === 'X-Pad-Import-Stale-Bodies' ? headerValue : null) },
			json: async () => ({ id: 'w1', name: 'Restored', slug: 'restored' })
		}))
	);
}

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('importBundle stale-body header', () => {
	const file = new File([new Uint8Array([1, 2, 3])], 'b.tar.gz');

	it('reports a real count', async () => {
		mockImportOnce('2');
		const ws = await api.workspaces.importBundle(file);
		expect(ws.stale_bodies).toBe(2);
	});

	it('leaves the field undefined when there is nothing to report', async () => {
		// THE CONTROL, and it covers the common case: a clean import must be
		// indistinguishable from one against a server that predates the header.
		for (const v of [null, '', '0']) {
			mockImportOnce(v);
			const ws = await api.workspaces.importBundle(file);
			expect(ws.stale_bodies, `header ${JSON.stringify(v)}`).toBeUndefined();
		}
	});

	it('rejects every spelling Go strconv.Atoi rejects', async () => {
		// Each of these is a value `Number()` alone would have accepted, and the
		// CLI suppresses: the divergence itself is the defect, because an
		// operator comparing the two surfaces would see different facts.
		for (const v of [
			'abc', '-1', 'NaN', '2.5', ' 7 ', '7e0', '0x7', '+7', '1e309', '9007199254740993',
			// A repeated header: Headers.get joins the values, and the Go side
			// refuses on len(Values) != 1 rather than reporting the first
			// (codex round 4 P2). The same response must say the same thing on
			// both surfaces.
			'2, 3',
			// An internal NUL, and the two values either side of the 2^53-1
			// ceiling both readers share.
			'2\u00003',
			'9007199254740992',
			'9223372036854775807'
		]) {
			mockImportOnce(v);
			const ws = await api.workspaces.importBundle(file);
			expect(ws.stale_bodies, `header ${JSON.stringify(v)} must be suppressed`).toBeUndefined();
		}
	});

	it('accepts leading zeros as the integer they spell, like Atoi', async () => {
		mockImportOnce('007');
		const ws = await api.workspaces.importBundle(file);
		expect(ws.stale_bodies).toBe(7);
	});

	it('accepts 2^53-1 exactly, the largest value both readers hold', async () => {
		// The boundary itself, so the ceiling above is a bound rather than an
		// off-by-one: the rejected cases are ABOVE this, not at it.
		mockImportOnce('9007199254740991');
		const ws = await api.workspaces.importBundle(file);
		expect(ws.stale_bodies).toBe(9007199254740991);
	});

	it('still returns the workspace itself', async () => {
		// The count is an advisory. A bundle with stale bodies still imports, so
		// the caller must get the workspace either way — asserting the absence of
		// stale_bodies above proves nothing if the whole result were undefined.
		mockImportOnce('2');
		const ws = await api.workspaces.importBundle(file);
		expect(ws.slug).toBe('restored');
		expect(ws.name).toBe('Restored');
	});
});
