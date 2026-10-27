import { describe, it, expect } from 'vitest';
import { readBoundedBytes, readBoundedText, NotStreamableError } from './boundedBytes';

// TASK-4075 F1: the shared bounded reader. Both loaders' suites cover it through
// their phases; these pin the reader's own contract.

function streamed(chunks: Uint8Array[], headers: Record<string, string> = {}) {
	const state = { reads: 0, cancelled: false };
	let i = 0;
	const reader = {
		read: async () => {
			state.reads++;
			return i < chunks.length ? { done: false, value: chunks[i++] } : { done: true, value: undefined };
		},
		cancel: async () => {
			state.cancelled = true;
		}
	};
	const response = {
		ok: true,
		headers: new Headers(headers),
		body: { getReader: () => reader, cancel: async () => { state.cancelled = true; } }
	} as unknown as Response;
	return { response, state };
}

describe('readBoundedBytes', () => {
	it('refuses ONE oversize chunk and cancels the reader', async () => {
		const { response, state } = streamed([new Uint8Array(1000)]);
		expect(await readBoundedBytes(response, 100)).toBeNull();
		expect(state.cancelled).toBe(true);
		expect(state.reads).toBe(1);
	});

	it('a declared Content-Length over max refuses before any read', async () => {
		const { response, state } = streamed([new Uint8Array(5)], { 'content-length': '101' });
		expect(await readBoundedBytes(response, 100)).toBeNull();
		expect(state.reads).toBe(0);
		expect(state.cancelled).toBe(true);
	});

	it('a declared Content-Length AT max passes the precheck', async () => {
		const { response } = streamed([new Uint8Array(5).fill(7)], { 'content-length': '100' });
		expect(await readBoundedBytes(response, 100)).toEqual(new Uint8Array(5).fill(7));
	});

	it('a missing, null-bodied or getReader-less body throws NotStreamableError', async () => {
		await expect(readBoundedBytes({ body: null } as unknown as Response, 10)).rejects.toBeInstanceOf(NotStreamableError);
		await expect(readBoundedBytes({ body: {} } as unknown as Response, 10)).rejects.toBeInstanceOf(NotStreamableError);
	});
});

describe('readBoundedText', () => {
	it('decodes across a multi-byte split and refuses past max', async () => {
		const bytes = new TextEncoder().encode('café');
		const { response } = streamed([bytes.slice(0, 4), bytes.slice(4)]);
		expect(await readBoundedText(response, 100)).toBe('café');
		const over = streamed([new Uint8Array(11)]);
		expect(await readBoundedText(over.response, 10)).toBeNull();
	});
});
