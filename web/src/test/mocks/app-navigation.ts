// Test-only stand-in for SvelteKit's `$app/navigation`.
//
// Same reason as `app-environment.ts` / `app-state.ts`: the jsdom vitest
// project runs without the SvelteKit vite plugin, so `$app/navigation` has no
// provider and every component importing it fails to RESOLVE — which is a
// load-time error `vi.mock` cannot rescue, since resolution happens first.
// That put whole components (`Sidebar`, `TopBar`, `PaneHost` and everything
// they pull in) out of reach of component tests entirely.
//
// Navigation is a NO-OP here rather than a spy: a test that wants to assert on
// it should `vi.mock('$app/navigation', …)` in its own file, which works fine
// once the specifier resolves at all. `goto` resolves so `await goto(...)`
// doesn't hang.

export async function goto(_url: string | URL, _opts?: unknown): Promise<void> {}
export async function invalidate(_resource?: unknown): Promise<void> {}
export async function invalidateAll(): Promise<void> {}
export async function preloadData(_href: string): Promise<unknown> {
	return { type: 'loaded', status: 200, data: {} };
}
export async function preloadCode(..._pathnames: string[]): Promise<void> {}
export function pushState(_url: string | URL, _state: unknown): void {}
export function replaceState(_url: string | URL, _state: unknown): void {}
export function beforeNavigate(_fn: unknown): void {}
export function afterNavigate(_fn: unknown): void {}
export function onNavigate(_fn: unknown): void {}
export function disableScrollHandling(): void {}
