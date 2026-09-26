// BUG-3052 unit 1: the prompt-variable context tolerates stored values String()
// cannot convert. `categoricalTemplateValue`, the `fields` line and plan/phase
// all converted raw values and threw on `{"toString":0}`.
import { describe, expect, it } from 'vitest';
import { contextFromItem } from './quick-action-preview';

describe('contextFromItem with a hostile stored value (BUG-3052)', () => {
	it('does not throw, and keeps the value visible as its JSON', () => {
		const item = {
			id: 'i1', title: 'T', content: '', item_number: 1,
			fields: '{"status":{"toString":0},"plan":{"toString":0},"note":"x"}',
		} as never;
		const collection = { slug: 'tasks', name: 'Tasks', prefix: 'TASK', schema: '{"fields":[]}' } as never;
		const ctx = contextFromItem(item, collection);
		expect(ctx.status).toBe('{"toString":0}');
		expect(ctx.plan).toBe('{"toString":0}');
		expect(ctx.fields).toContain('status: {"toString":0}');
		expect(ctx.fields).toContain('note: x');
	});
});
