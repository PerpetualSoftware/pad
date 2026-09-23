// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`), which
// compiles the `.svelte.ts` rune module and aliases `$app/environment` to a
// browser=true mock (see vitest.config.ts). `breakpoint.svelte.ts` reads
// `window.matchMedia` at import time and wires a `change` listener, so each
// test installs a controllable matchMedia mock BEFORE importing the module and
// resets the module registry between cases.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

type ChangeListener = (e: Pick<MediaQueryListEvent, 'matches'>) => void;

/**
 * Install a fake `window.matchMedia` whose `MediaQueryList` starts at
 * `initialMatches` and lets the test fire synthetic `change` events. Returns
 * the media query the module asked about plus a `fireChange` trigger.
 */
function installMatchMedia(initialMatches: boolean) {
	const listeners = new Set<ChangeListener>();
	const mql = {
		matches: initialMatches,
		media: '',
		onchange: null,
		addEventListener: (_type: string, cb: ChangeListener) => listeners.add(cb),
		removeEventListener: (_type: string, cb: ChangeListener) => listeners.delete(cb),
		// Legacy Safari API — harmless to provide.
		addListener: (cb: ChangeListener) => listeners.add(cb),
		removeListener: (cb: ChangeListener) => listeners.delete(cb),
		dispatchEvent: () => true
	};
	const matchMedia = vi.fn((query: string) => {
		mql.media = query;
		return mql;
	});
	vi.stubGlobal('matchMedia', matchMedia);
	return {
		matchMedia,
		fireChange(matches: boolean) {
			for (const cb of listeners) cb({ matches });
		}
	};
}

beforeEach(() => {
	vi.resetModules();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('viewport.isMobile', () => {
	it('is true when the viewport starts below the breakpoint', async () => {
		installMatchMedia(true);
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.isMobile).toBe(true);
	});

	it('is false when the viewport starts at/above the breakpoint', async () => {
		installMatchMedia(false);
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.isMobile).toBe(false);
	});

	it('queries the canonical 768px media query', async () => {
		const ctl = installMatchMedia(false);
		const { MOBILE_MEDIA_QUERY, MOBILE_BREAKPOINT } = await import('./breakpoint.svelte');
		expect(MOBILE_BREAKPOINT).toBe(768);
		expect(MOBILE_MEDIA_QUERY).toBe('(max-width: 768px)');
		expect(ctl.matchMedia).toHaveBeenCalledWith('(max-width: 768px)');
	});

	it('reacts to a simulated viewport change event', async () => {
		const ctl = installMatchMedia(false);
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.isMobile).toBe(false);

		ctl.fireChange(true);
		expect(viewport.isMobile).toBe(true);

		ctl.fireChange(false);
		expect(viewport.isMobile).toBe(false);
	});
});

/**
 * BUG-3158 — a matchMedia whose answer depends on the QUERY, so width and
 * pointer can disagree (a landscape phone: wide AND coarse). The installer
 * above answers every query alike and cannot express that case.
 */
function installPerQuery(initial: Record<string, boolean>) {
	const lists = new Map<string, { matches: boolean; listeners: Set<ChangeListener> }>();
	vi.stubGlobal(
		'matchMedia',
		vi.fn((query: string) => {
			if (!lists.has(query)) lists.set(query, { matches: initial[query] ?? false, listeners: new Set() });
			const l = lists.get(query)!;
			return {
				get matches() {
					return l.matches;
				},
				media: query,
				onchange: null,
				addEventListener: (_t: string, cb: ChangeListener) => l.listeners.add(cb),
				removeEventListener: (_t: string, cb: ChangeListener) => l.listeners.delete(cb),
				addListener: (cb: ChangeListener) => l.listeners.add(cb),
				removeListener: (cb: ChangeListener) => l.listeners.delete(cb),
				dispatchEvent: () => true,
			};
		}),
	);
	return {
		fire(query: string, matches: boolean) {
			const l = lists.get(query)!;
			l.matches = matches;
			for (const cb of l.listeners) cb({ matches });
		},
	};
}

describe('viewport.dragDisabled (BUG-3158)', () => {
	const WIDE = '(max-width: 768px)';
	const COARSE = '(pointer: coarse)';

	it('a landscape phone — wide but coarse — has drag disabled', async () => {
		installPerQuery({ [WIDE]: false, [COARSE]: true });
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.isMobile, 'precondition: not mobile by width').toBe(false);
		expect(viewport.isCoarsePointer).toBe(true);
		expect(viewport.dragDisabled).toBe(true);
	});

	it('a narrow viewport with a fine pointer keeps the width rule the board always had', async () => {
		installPerQuery({ [WIDE]: true, [COARSE]: false });
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.dragDisabled).toBe(true);
	});

	it('CONTROL: a wide viewport with a fine pointer (desktop, touch laptop) keeps drag', async () => {
		installPerQuery({ [WIDE]: false, [COARSE]: false });
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.dragDisabled).toBe(false);
	});

	it('follows a pointer change (a tablet docked to a mouse, and back)', async () => {
		const ctl = installPerQuery({ [WIDE]: false, [COARSE]: true });
		const { viewport } = await import('./breakpoint.svelte');
		expect(viewport.dragDisabled).toBe(true);
		ctl.fire(COARSE, false);
		expect(viewport.dragDisabled).toBe(false);
		ctl.fire(COARSE, true);
		expect(viewport.dragDisabled).toBe(true);
	});
});
