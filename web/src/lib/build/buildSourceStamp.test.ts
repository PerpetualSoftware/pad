import { describe, it, expect } from 'vitest';
import { buildSourceStamp, type GitRunner } from './buildSourceStamp';

function runner(outputs: Record<string, string | Error>): GitRunner {
	return (args) => {
		const out = outputs[args[0]];
		if (out instanceof Error) throw out;
		if (out === undefined) throw new Error(`unexpected git ${args.join(' ')}`);
		return out;
	};
}

describe('buildSourceStamp (TASK-3233)', () => {
	it('a clean tree stamps the commit and zero', () => {
		expect(buildSourceStamp(runner({ 'rev-parse': 'abc123\n', status: '' }))).toEqual({ commit: 'abc123', dirty: 0 });
	});

	it('counts each porcelain entry under web/', () => {
		const status = ' M src/lib/components/items/CopyItemDialog.svelte\n?? src/new.ts\n';
		expect(buildSourceStamp(runner({ 'rev-parse': 'abc123', status }))).toEqual({ commit: 'abc123', dirty: 2 });
	});

	it('no git: both fields null (unknown, never clean)', () => {
		expect(buildSourceStamp(runner({ 'rev-parse': new Error('not a git repository') }))).toEqual({
			commit: null,
			dirty: null
		});
	});

	it('a failing status leaves dirty unknown, not zero', () => {
		expect(buildSourceStamp(runner({ 'rev-parse': 'abc123', status: new Error('boom') }))).toEqual({
			commit: 'abc123',
			dirty: null
		});
	});
});
