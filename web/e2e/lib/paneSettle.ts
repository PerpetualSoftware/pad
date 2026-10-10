import type { Page } from '@playwright/test';

/** `PANE_MINT_SETTLE_MS` (src/lib/collections/paneMintSettle.ts). */
export const PANE_MINT_SETTLE_MS = 140;

/** How long after the Back's popstate the settle may take to arm. */
const ARM_WINDOW_MS = 1000;

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
 * AFTER the Back's traversal reached this helper's popstate listener, and
 * within ARM_WINDOW_MS of it. SvelteKit 3's popstate handler is async: it
 * resolves the navigation across tasks and reaches afterNavigate (where the
 * settle is armed) AFTER every popstate listener has run (measured on
 * TASK-3423, 13 runs: our listener at 3-6ms, the arm 2-30ms after it, past a
 * 0ms task and sometimes past a rAF). Under kit 2 the arm ran inside the
 * dispatch, before this listener, which is what the window used to assume.
 * The pane-follow debounce arms only from a j/k keydown, and no input can
 * interleave this single page.evaluate, so nothing else arms a 140ms timer in
 * the window. A timer armed before the traversal is never taken; if no settle
 * arms within ARM_WINDOW_MS, the helper rejects rather than waiting on a
 * timer that may not be the settle.
 */
export async function backThenDrillAfterSettleArms(
	page: Page,
	ref: string,
	delayMs: number,
): Promise<{ drillAfterArmMs: number; settleFiredFirst: boolean }> {
	return page.evaluate(
		([r, delay, settleMs, armWindowMs]) =>
			new Promise<{ drillAfterArmMs: number; settleFiredFirst: boolean }>((resolve, reject) => {
				const orig = window.setTimeout;
				const restore = () => {
					window.setTimeout = orig;
				};
				let fired = false;
				let inArmWindow = false;
				window.setTimeout = ((fn: TimerHandler, ms?: number, ...rest: unknown[]) => {
					if (!inArmWindow || ms !== settleMs || typeof fn !== 'function') return orig(fn, ms, ...rest);
					restore();
					inArmWindow = false;
					clearTimeout(armDeadline);
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
				let armDeadline: ReturnType<typeof setTimeout> | undefined;
				const onPopstate = () => {
					// The traversal has happened; SvelteKit's async handling of it,
					// settle included, is still to come: the window opens here.
					clearTimeout(safety);
					inArmWindow = true;
					armDeadline = orig(() => {
						if (window.setTimeout === orig) return;
						inArmWindow = false;
						restore();
						reject(new Error(`the back-settle was not armed within ${armWindowMs}ms of the Back's popstate`));
					}, armWindowMs);
				};
				window.addEventListener('popstate', onPopstate, { once: true });
				const safety = orig(() => {
					window.removeEventListener('popstate', onPopstate);
					restore();
					reject(new Error('no popstate within 5s of history.back()'));
				}, 5000);
				history.back();
			}),
		[ref, delayMs, PANE_MINT_SETTLE_MS, ARM_WINDOW_MS] as const,
	);
}
