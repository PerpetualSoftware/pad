import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-4885: browserLogin must leave the page on a document that runs no app
 * code, so nothing navigates behind the caller's next page.goto.
 *
 * The suite's context sends a Bearer token on every request, so an app page
 * believes it is signed in. When the helper logged in from /login, the app
 * there moved itself to `/`, and under a loaded runner a home-page request
 * that went out before the login cookie landed came back 401 and sent the
 * page to /login?redirect=%2F with a hard navigation. A caller's goto issued
 * in that window was superseded (net::ERR_ABORTED) and waited for a load
 * event that never came, until the test timed out.
 */
test('browserLogin leaves the page where it landed, with the session cookie set', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the helper is viewport-independent; one project is enough');
	const navigations: string[] = [];
	page.on('framenavigated', (f) => {
		if (f === page.mainFrame()) navigations.push(f.url());
	});

	await browserLogin(page);
	const landed = page.url();
	// Long enough for the app at /login to have moved itself (it did so within
	// a second on an idle machine); nothing on the landing document can move it.
	await page.waitForTimeout(2_000);

	expect(navigations, 'main-frame navigations: only the helper\'s own landing').toEqual([landed]);
	expect(page.url()).toBe(landed);
	const cookies = await page.context().cookies();
	expect(cookies.some((c) => /(^|__Host-)pad_session$/.test(c.name)), 'the in-page login set the session cookie').toBe(true);
});
