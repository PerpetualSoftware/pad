// BUG-3075: the playbook editor's save sends only the keys the user changed.
import { describe, expect, it } from 'vitest';
import { playbookFieldsPatch, storedFormMismatches, type PlaybookFormSnapshot } from './editorPatch';

const loaded: PlaybookFormSnapshot = {
	status: 'draft',
	trigger: 'manual',
	scope: 'all',
	invocationSlug: 'ship',
	args: '[]',
};

describe('playbookFieldsPatch', () => {
	it('an untouched form sends no field at all', () => {
		// The defect: a stored non-string status loads as the DEFAULT `draft`,
		// and the old save sent it back, overwriting the stored value unasked.
		expect(playbookFieldsPatch({ ...loaded }, loaded)).toEqual({});
	});

	it('sends exactly the key the user changed', () => {
		expect(playbookFieldsPatch({ ...loaded, status: 'active' }, loaded)).toEqual({ status: 'active' });
		expect(playbookFieldsPatch({ ...loaded, trigger: 'on-release' }, loaded)).toEqual({ trigger: 'on-release' });
		expect(playbookFieldsPatch({ ...loaded, scope: 'backend' }, loaded)).toEqual({ scope: 'backend' });
	});

	it('arguments go as a JSON value, only when changed', () => {
		const args = '[{"name":"target","type":"ref","required":true}]';
		expect(playbookFieldsPatch({ ...loaded, args }, loaded)).toEqual({ arguments: JSON.parse(args) });
	});

	it('a cleared slug is a null (the unique index refuses ""), and padding alone is no change', () => {
		expect(playbookFieldsPatch({ ...loaded, invocationSlug: '  ' }, loaded)).toEqual({ invocation_slug: null });
		expect(playbookFieldsPatch({ ...loaded, invocationSlug: ' ship ' }, loaded)).toEqual({});
		expect(playbookFieldsPatch({ ...loaded, invocationSlug: 'release' }, loaded)).toEqual({ invocation_slug: 'release' });
	});

	it('with nothing loaded to compare against, every key is sent (the old behaviour)', () => {
		expect(Object.keys(playbookFieldsPatch(loaded, null)).sort()).toEqual(
			['arguments', 'invocation_slug', 'scope', 'status', 'trigger'],
		);
	});
});

describe('storedFormMismatches', () => {
	const statuses = ['draft', 'active', 'deprecated'];
	const scopes = ['all', 'backend'];

	it('names a non-string value and a non-option status or scope, with its raw text', () => {
		expect(
			storedFormMismatches({ status: 5, trigger: 'manual', scope: 'nowhere' }, statuses, scopes),
		).toEqual([
			{ label: 'Status', raw: '5' },
			{ label: 'Scope', raw: '"nowhere"' },
		]);
		// A relation id where the status was retyped: a string, but no option.
		expect(storedFormMismatches({ status: 'item-uuid', trigger: undefined, scope: undefined }, statuses, scopes)).toEqual([
			{ label: 'Status', raw: '"item-uuid"' },
		]);
	});

	it('a custom trigger string is legitimate; only a non-string trigger is named', () => {
		expect(storedFormMismatches({ status: 'draft', trigger: 'on-anything', scope: 'all' }, statuses, scopes)).toEqual([]);
		expect(storedFormMismatches({ status: 'draft', trigger: ['x'], scope: 'all' }, statuses, scopes)).toEqual([
			{ label: 'Trigger', raw: '["x"]' },
		]);
	});

	it('absent values are not mismatches', () => {
		expect(storedFormMismatches({ status: undefined, trigger: null, scope: '' }, statuses, scopes)).toEqual([]);
	});
});
