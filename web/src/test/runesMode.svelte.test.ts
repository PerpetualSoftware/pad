/**
 * TASK-3423: component tests compile in runes mode.
 *
 * The forcing used to reach this project only because vite-plugin-svelte
 * loads svelte.config.js by itself. That file is gone (SvelteKit 3 refuses
 * it), so vitest.config.ts now passes the shared dynamicCompileOptions to the
 * jsdom project's svelte() explicitly. If it stops doing so, a file with no
 * runes in it compiles in LEGACY mode again, and nothing else would notice.
 *
 * A legacy-only construct (`$:`) is a compile error in runes mode, so the
 * proof is that importing such a file is REFUSED. The fixture is written at
 * test time rather than checked in (and imported by a computed path, which
 * Vite cannot resolve before the file exists): as a tracked .svelte file it
 * would fail svelte-check and the app build, which compile in runes mode too.
 */
import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import { mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { resolve } from 'node:path';

// jsdom's import.meta.url is not a file: URL; vitest runs from web/.
const dir = resolve(process.cwd(), 'src/test/.runes-fixture') + '/';

beforeAll(() => {
	mkdirSync(dir, { recursive: true });
	writeFileSync(dir + 'LegacyOnly.svelte', '<script>\n\tlet count = 1;\n\t$: doubled = count * 2;\n</script>\n\n<p>{doubled}</p>\n');
	writeFileSync(dir + 'Plain.svelte', '<script>\n\tlet count = $state(1);\n</script>\n\n<p>{count}</p>\n');
});

afterAll(() => {
	rmSync(dir, { recursive: true, force: true });
});

describe('component tests compile in runes mode (TASK-3423)', () => {
	it('a legacy reactive statement is refused', async () => {
		await expect(import(/* @vite-ignore */ dir + 'LegacyOnly.svelte')).rejects.toThrow(/legacy_reactive_statement_invalid|runes mode/);
	});

	it('control: a runes component from the same directory compiles', async () => {
		const mod = await import(/* @vite-ignore */ dir + 'Plain.svelte');
		expect(mod.default).toBeTypeOf('function');
	});
});
