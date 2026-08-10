// Setup for the jsdom vitest project (see vitest.config.ts). Only loaded when
// the browser test deps are installed, so importing them here is safe.
import '@testing-library/jest-dom/vitest';
import { beforeEach } from 'vitest';

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

// This jsdom build exposes a `localStorage` OBJECT with no working methods
// (it warns `--localstorage-file was provided without a valid path`), so
// `localStorage.getItem is not a function` blows up at MODULE-LOAD time for
// every store that reads a persisted preference — `ui.svelte.ts` does, which
// takes `Sidebar`/`TopBar` and everything importing them out of reach of
// component tests entirely (TASK-2430). A `typeof … !== 'function'` probe
// (not a truthiness check on the object) is what actually detects it.
//
// SCOPE: do NOT assume a fresh instance per file. Whether the jsdom environment
// (and therefore this shim) is rebuilt per test file depends on vitest's pool /
// isolation config, and the `getItem` probe below deliberately skips
// re-installation when a working Storage already exists — so a shim installed
// for an earlier file can survive into a later one, carrying its keys with it.
// That would make FILE ORDER observable through any module that reads a
// persisted preference (`ui.svelte.ts` reads `pad-topbar` at import time, and
// `PaneHost` persists a pane width on every drag).
//
// This repo runs plain `npm run test` with vitest's DEFAULT per-file isolation
// (no `--no-isolate`, no pool override, in the config or in CI — checked), but
// the `beforeEach` clear below does not rely on that: storage is emptied before
// every TEST, so the shim stays deterministic if the pool config ever changes.
// A test that needs a seeded value seeds it in its own `beforeEach`, which runs
// after this one. Seeding in `beforeAll` will NOT survive — deliberately, since
// that is precisely the pattern that makes order matter.
//
// TRADE-OFF WORTH KNOWING: this replaces BOTH `localStorage` and
// `sessionStorage` with in-memory stand-ins. They are dumb maps — no quota, no
// `StorageEvent`, no cross-document semantics, no serialization of non-strings
// beyond `String(v)`. A genuine persistence bug that only reproduces against
// real browser Storage will therefore pass here. If you are chasing one, that
// is the first thing to rule out; the honest test for it is Playwright.
if (typeof globalThis.localStorage?.getItem !== 'function') {
	const makeStorage = (): Storage => {
		const map = new Map<string, string>();
		return {
			get length() {
				return map.size;
			},
			key: (i: number) => Array.from(map.keys())[i] ?? null,
			getItem: (k: string) => (map.has(k) ? (map.get(k) as string) : null),
			setItem: (k: string, v: string) => void map.set(k, String(v)),
			removeItem: (k: string) => void map.delete(k),
			clear: () => map.clear(),
		} as Storage;
	};
	Object.defineProperty(globalThis, 'localStorage', {
		value: makeStorage(),
		configurable: true,
		writable: true,
	});
	Object.defineProperty(globalThis, 'sessionStorage', {
		value: makeStorage(),
		configurable: true,
		writable: true,
	});
}

// Unconditional, and per TEST rather than per setup-file load: covers BOTH
// branches above (freshly installed shim, or a real / previously-installed
// Storage the probe skipped over) under any isolation setting.
globalThis.localStorage?.clear?.();
globalThis.sessionStorage?.clear?.();
beforeEach(() => {
	globalThis.localStorage?.clear?.();
	globalThis.sessionStorage?.clear?.();
});

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

// jsdom has no `ResizeObserver`. The attachment viewer (`Lightbox`) observes its
// stage to re-clamp the zoom on viewport changes, and several suites mount it —
// directly (`Lightbox.svelte.test.ts`) and through its producers
// (`AttachmentSurfaceHost`, `ItemTimeline`, `ItemAttachmentStrip`). The component
// GUARDS on `typeof ResizeObserver === 'undefined'` (for SSR), so without this
// shim it would not throw — it would simply SKIP the observer branch entirely,
// leaving the resize path untested and unexercised in every suite that mounts
// the viewer. The shim makes that branch actually run (construct + observe). The
// repo's only prior shim is LOCAL to `TopBar.svelte.test.ts` (and DRIVES a size,
// to force overflow) — this global one is deliberately INERT: jsdom lays nothing
// out, so there is no size to report, and firing a zero-rect callback would only
// exercise a no-op clamp. A test that needs to DRIVE a resize installs its own
// via `vi.stubGlobal`, which layers over this default and is cleared by
// `vi.unstubAllGlobals()`.
if (typeof globalThis.ResizeObserver === 'undefined') {
	class ResizeObserverShim {
		observe(): void {}
		unobserve(): void {}
		disconnect(): void {}
	}
	globalThis.ResizeObserver = ResizeObserverShim as unknown as typeof ResizeObserver;
}
