/**
 * TASK-3423: the app build compiles our files in runes mode.
 *
 * The Svelte config moved from svelte.config.js into vite.config.ts's
 * sveltekit({...}), which forwards dynamicCompileOptions to vite-plugin-svelte.
 * This resolves the REAL vite.config.ts the way a build does and reads the
 * options vite-plugin-svelte ended up with, so a config that drops or reshapes
 * the forwarding fails here rather than in a build that quietly compiles
 * legacy components.
 */
import { describe, it, expect } from 'vitest';
import { resolveConfig, type Plugin } from 'vite';
import { fileURLToPath } from 'node:url';
import { dynamicCompileOptions } from '../../svelteCompileOptions';

const root = fileURLToPath(new URL('../../', import.meta.url));

type SvelteConfigPlugin = Plugin & { api?: { options?: Record<string, unknown> } };

describe('vite.config.ts carries the Svelte compile options (TASK-3423)', () => {
	it('vite-plugin-svelte resolves our dynamicCompileOptions and no svelte.config.js', async () => {
		const config = await resolveConfig({ root, configFile: root + 'vite.config.ts', logLevel: 'silent' }, 'build', 'production');
		const plugin = config.plugins.find((p) => p.name === 'vite-plugin-svelte:config') as SvelteConfigPlugin | undefined;
		expect(plugin, 'vite-plugin-svelte is not in the resolved plugin list').toBeDefined();
		const options = plugin!.api?.options;
		expect(options, 'vite-plugin-svelte resolved no options').toBeDefined();

		const resolved = options!.dynamicCompileOptions as typeof dynamicCompileOptions | undefined;
		expect(resolved, 'dynamicCompileOptions did not reach vite-plugin-svelte').toBeTypeOf('function');
		expect(resolved!({ filename: root + 'src/lib/components/Anything.svelte' })).toEqual({ runes: true });
		expect(resolved!({ filename: root + 'node_modules/some-lib/Thing.svelte' })).toBeUndefined();
		expect(options!.configFile).toBe(false);
	}, 60_000);
});
