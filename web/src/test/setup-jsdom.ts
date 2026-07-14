// Setup for the jsdom vitest project (see vitest.config.ts). Only loaded when
// the browser test deps are installed, so importing them here is safe.
import '@testing-library/jest-dom/vitest';

// jsdom does not implement the native <dialog> top-layer methods
// (`HTMLDialogElement.prototype.showModal` / `.close` are either missing or
// throw "Not implemented"). Modal.svelte drives the element through those, so
// polyfill a minimal, spec-shaped version that just reflects the `open`
// attribute + fires the `close` event. This exercises the component's
// open/close logic without a real UA.
if (typeof HTMLDialogElement !== 'undefined') {
	HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
		this.open = true;
		this.setAttribute('open', '');
	};
	HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement, returnValue?: string) {
		if (returnValue !== undefined) {
			this.returnValue = returnValue;
		}
		this.open = false;
		this.removeAttribute('open');
		this.dispatchEvent(new Event('close'));
	};
}

// jsdom does not implement `window.matchMedia`. Several client-only stores
// (`breakpoint.svelte.ts`, `ui.svelte.ts`) call it at MODULE-LOAD time,
// guarded behind SvelteKit's `browser` flag — which the jsdom test project
// forces `true` (see `src/test/mocks/app-environment.ts`). That means any
// jsdom test whose component tree transitively imports one of those stores
// (e.g. FilterBar.svelte → breakpoint.svelte.ts) needs `matchMedia` to exist
// before its top-level `import` runs; a per-test `vi.stubGlobal` (as
// `breakpoint.svelte.test.ts` uses for a controllable result) is too late
// for a static import. This default polyfill (match-nothing / desktop
// viewport) covers everyone else; tests that need a specific result install
// their own `vi.stubGlobal('matchMedia', ...)`, which layers over — and
// `vi.unstubAllGlobals()` cleanly restores — this default.
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
	window.matchMedia = function matchMedia(query: string): MediaQueryList {
		return {
			matches: false,
			media: query,
			onchange: null,
			addEventListener: () => {},
			removeEventListener: () => {},
			addListener: () => {},
			removeListener: () => {},
			dispatchEvent: () => false,
		} as MediaQueryList;
	};
}
