import { test, expect } from '@playwright/test';

// THROWAWAY (BUG-3278): fails its first attempt and passes its retry, so CI
// sees exactly one flaky test and the flaky-evidence upload must fire. Never merge.
test('BUG-3278 flaky-evidence probe', async ({ page }, testInfo) => {
	await page.setContent('<p>probe</p>');
	expect(testInfo.retry, 'first attempt fails on purpose').toBeGreaterThan(0);
});
