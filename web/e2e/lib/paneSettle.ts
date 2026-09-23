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
				window.setTimeout = ((fn: TimerHandler, ms?: number, ...rest: unknown[]) => {
					if (ms !== settleMs || typeof fn !== 'function') return orig(fn, ms, ...rest);
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
				orig(() => {
					restore();
					reject(new Error('the back-settle was never armed'));
				}, 5000);
				history.back();
			}),
		[ref, delayMs, PANE_MINT_SETTLE_MS] as const,
	);
}
