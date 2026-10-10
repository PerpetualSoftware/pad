import { describe, expect, it } from 'vitest';
import { uniqueBy, uniqueStrings } from './unique';
import { normalizeSchemaForRender, parseSchema, parseStoredSchema, schemaRenderRepairs, type Collection } from '$lib/types';

// TASK-3539: the helpers that keep a keyed {#each} from throwing
// each_key_duplicate on a repeated key.

describe('uniqueBy / uniqueStrings', () => {
	it('keeps the first entry for each key, in order', () => {
		const rows = [{ id: 'a', n: 1 }, { id: 'b', n: 2 }, { id: 'a', n: 3 }];
		expect(uniqueBy(rows, (r) => r.id)).toEqual([{ id: 'a', n: 1 }, { id: 'b', n: 2 }]);
		expect(uniqueStrings(['x', 'y', 'x', 'z', 'y'])).toEqual(['x', 'y', 'z']);
		expect(uniqueStrings([])).toEqual([]);
	});
});

describe('parseSchema normalises a stored schema for rendering', () => {
	const coll = (schema: unknown) => ({ schema: JSON.stringify(schema) }) as unknown as Collection;

	it('keeps the first definition of a repeated field key', () => {
		const s = parseSchema(
			coll({
				fields: [
					{ key: 'status', label: 'Status', type: 'select', options: ['open'] },
					{ key: 'status', label: 'Other', type: 'text' },
					{ key: 'effort', label: 'Effort', type: 'text' },
				],
			}),
		);
		expect(s.fields.map((f) => [f.key, f.label])).toEqual([
			['status', 'Status'],
			['effort', 'Effort'],
		]);
	});

	it('dedupes a select field’s options and drops the empty one', () => {
		const s = parseSchema(
			coll({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', '', 'done', 'open'] }] }),
		);
		expect(s.fields[0].options).toEqual(['open', 'done']);
	});

	it('leaves a clean schema as it was, and survives junk', () => {
		const clean = { fields: [{ key: 'a', label: 'A', type: 'select', options: ['x', 'y'] }] };
		expect(parseSchema(coll(clean))).toEqual(clean);
		expect(parseSchema({ schema: 'not json' } as unknown as Collection)).toEqual({ fields: [] });
		expect(normalizeSchemaForRender({ fields: [null, 3, { key: 'k', label: 'K', type: 'text' }] } as never).fields).toEqual([
			{ key: 'k', label: 'K', type: 'text' },
		]);
	});
});

describe('write paths keep the stored schema; editors name the repairs (codex round 1)', () => {
	const coll = (schema: unknown) => ({ schema: JSON.stringify(schema) }) as unknown as Collection;
	const broken = {
		fields: [
			{ key: 'status', label: 'Status', type: 'select', options: ['open', '', 'open', 'done'] },
			{ key: 'status', label: 'Again', type: 'text' },
		],
	};

	it('parseStoredSchema returns the schema exactly as stored', () => {
		expect(parseStoredSchema(coll(broken))).toEqual(broken);
	});

	it('schemaRenderRepairs names each thing the normalisation removes', () => {
		expect(schemaRenderRepairs(coll(broken))).toEqual([
			'Field "status" has an empty option, which is removed.',
			'Field "status" lists the option "open" more than once; it is kept once.',
			'Field "status" is defined more than once; only the first definition is kept.',
		]);
		expect(schemaRenderRepairs(coll({ fields: [{ key: 'a', label: 'A', type: 'select', options: ['x'] }] }))).toEqual([]);
	});
});
