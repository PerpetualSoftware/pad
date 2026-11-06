// TASK-4315: the Chat sanitizer profile must not change what the item renderer
// produces. The expected outputs in markdown.itemPin.fixture.json were generated
// against the item renderer BEFORE the Chat profile existed; regenerate only on
// a deliberate item-renderer change: PAD_UPDATE_ITEM_PIN=1 npx vitest run <this file>.
import { describe, it, expect } from 'vitest';
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { renderMarkdown, sanitizeMarkdownHtml } from './markdown';
import { ITEM_PIN_INPUTS } from './markdown.itemPin.inputs';

const FIXTURE = resolve(process.cwd(), 'src/lib/utils/markdown.itemPin.fixture.json');


function compute(): Record<string, { sanitize: string; render: string }> {
	const out: Record<string, { sanitize: string; render: string }> = {};
	for (const [name, input] of Object.entries(ITEM_PIN_INPUTS)) {
		out[name] = { sanitize: sanitizeMarkdownHtml(input), render: renderMarkdown(input, [], 'ws') };
	}
	return out;
}

describe('item renderer output is pinned', () => {
	it('matches the fixture generated before the Chat profile', () => {
		if (process.env.PAD_UPDATE_ITEM_PIN) {
			writeFileSync(FIXTURE, JSON.stringify(compute(), null, '\t') + '\n');
		}
		const expected = JSON.parse(readFileSync(FIXTURE, 'utf8'));
		expect(compute()).toEqual(expected);
	});
});
