import { expect, it } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

const src = resolve(__dirname, '../../..');
function svelteFiles(directory: string): string[] {
	return readdirSync(directory, { withFileTypes: true }).flatMap(entry => {
		const path = join(directory, entry.name);
		return entry.isDirectory() ? svelteFiles(path) : entry.name.endsWith('.svelte') ? [path] : [];
	});
}

it('keeps the sole month grid in MonthGrid.svelte', () => {
	expect(svelteFiles(src).filter(path => readFileSync(path, 'utf8').includes('role="grid"')))
		.toEqual([join(src, 'lib/components/calendar/MonthGrid.svelte')]);
});
