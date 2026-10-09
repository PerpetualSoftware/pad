// TASK-3520: the ref-counted "an overlay covers the page" signal.
import { describe, it, expect, beforeEach } from 'vitest';
import { coveredPage, __resetCoveredPageForTests } from './coveredPage.svelte';

beforeEach(() => __resetCoveredPageForTests());

describe('coveredPage', () => {
	it('is active while any overlay is entered', () => {
		expect(coveredPage.active).toBe(false);
		const leave = coveredPage.enter({ keepBottomNav: false });
		expect(coveredPage.active).toBe(true);
		leave();
		expect(coveredPage.active).toBe(false);
	});

	it('keeps the bottom nav live while ANY covering overlay asks for it', () => {
		const panel = coveredPage.enter({ keepBottomNav: false });
		expect(coveredPage.keepsBottomNav).toBe(false);
		const sheet = coveredPage.enter({ keepBottomNav: true });
		expect(coveredPage.keepsBottomNav).toBe(true);
		sheet();
		expect(coveredPage.keepsBottomNav).toBe(false);
		expect(coveredPage.active).toBe(true);
		panel();
		expect(coveredPage.active).toBe(false);
	});

	it('an overlapping close and open cannot clear the other overlay early', () => {
		const a = coveredPage.enter({ keepBottomNav: true });
		const b = coveredPage.enter({ keepBottomNav: true });
		a();
		expect(coveredPage.active).toBe(true);
		b();
		expect(coveredPage.active).toBe(false);
	});

	it('leaving twice is a no-op', () => {
		const a = coveredPage.enter({ keepBottomNav: false });
		const b = coveredPage.enter({ keepBottomNav: false });
		a();
		a();
		expect(coveredPage.active).toBe(true);
		b();
		expect(coveredPage.active).toBe(false);
	});
});
