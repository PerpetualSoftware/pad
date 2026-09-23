import type { Page } from '@playwright/test';

/** `PANE_MINT_SETTLE_MS` (src/lib/collections/paneMintSettle.ts). */
export const PANE_MINT_SETTLE_MS = 140;

/**
 * Browser Back, then a pane drill `delayMs` after the Back's mint-settle is
 * ARMED — both timed in the page (BUG-3166), so no Playwright round-trip sits
 * between them. The settle is recognised as the `PANE_MINT_SETTLE_MS`
 * setTimeout the Back's afterNavigate arms; its callback is wrapped to record
 * whether it had FIRED by the time the drill ran, which is the precondition
 * that decides what the drill may observe (a busy main thread delays both
 * timers, so the delay alone does not say which ran first).
 *
 * Which 140ms timer is the settle (codex r1): the collection host's j/k
 * pane-follow debounce is ALSO 140ms, so the wrapper latches only a timer armed
 * INSIDE the Back's popstate dispatch — after `history.back()` is called and
 * before this helper's own popstate listener runs. SvelteKit's listener was
 * registered first and its navigation resolves in microtasks, which run to
 * afterNavigate (where the settle is armed) before the next listener (measured:
 * armed at 5ms, our listener at 6ms). A timer armed before the Back is never
 * taken; if a future change moves the arming out of that window, the helper
 * rejects rather than latching the wrong timer.
 */
export async function backThenDrillAfterSettleArms(
	page: Page,
	ref: string,
	delayMs: number,
): Promise<{ drillAfterArmMs: number; settleFiredFirst: boolean }> {
	return page.evaluate(
		([r, delay, settleMs]) =>
			new Promise<{ drillAfterArmMs: number; settleFiredFirst: boolean }>((resolve, reject) => {
				const orig = window.setTimeout;
				const restore = () => {
					window.setTimeout = orig;
				};
				let fired = false;
				let inBackDispatch = false;
				window.setTimeout = ((fn: TimerHandler, ms?: number, ...rest: unknown[]) => {
					if (!inBackDispatch || ms !== settleMs || typeof fn !== 'function') return orig(fn, ms, ...rest);
					restore();
					const armed = performance.now();
					const settle = fn as (...a: unknown[]) => void;
					const id = orig(
						(...a: unknown[]) => {
							fired = true;
							settle(...a);
						},
						ms,
						...rest,
					);
					orig(() => {
						const settleFiredFirst = fired;
						(
							window as unknown as { __padPaneController?: { navigatePaneTo(x: string): void } }
						).__padPaneController?.navigatePaneTo(r);
						resolve({ drillAfterArmMs: performance.now() - armed, settleFiredFirst });
					}, delay);
					return id;
				}) as typeof window.setTimeout;
				const onPopstate = () => {
					// Registered after SvelteKit's, so this runs once its handling of
					// the Back (settle included) is done: the window closes here.
					clearTimeout(safety);
					inBackDispatch = false;
					if (window.setTimeout !== orig) {
						restore();
						reject(new Error('the back-settle was not armed inside the popstate dispatch'));
					}
				};
				window.addEventListener('popstate', onPopstate, { once: true });
				const safety = orig(() => {
					window.removeEventListener('popstate', onPopstate);
					inBackDispatch = false;
					restore();
					reject(new Error('no popstate within 5s of history.back()'));
				}, 5000);
				inBackDispatch = true;
				history.back();
			}),
		[ref, delayMs, PANE_MINT_SETTLE_MS] as const,
	);
}
