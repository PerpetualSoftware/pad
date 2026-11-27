import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-4885: browserLogin must leave the page on a document that runs no app
 * code, so nothing navigates behind the caller's next page.goto.
 *
 * The suite's context is already signed in (a session cookie by default, a
 * Bearer header under `pageAuth: 'token'`), so an app page knows it is signed
 * in. When the helper logged in from /login, the app there moved itself to
 * `/`, and under a loaded runner a home-page request racing the helper's own
 * login came back 401 and sent the page to /login?redirect=%2F with a hard
 * navigation. A caller's goto issued in that window was superseded
 * (net::ERR_ABORTED) and waited for a load event that never came, until the
 * test timed out.
 */
const SESSION_COOKIE = /(^|__Host-)pad_session$/;

for (const pageAuth of ['session', 'token'] as const) {
	test.describe(`pageAuth: ${pageAuth}`, () => {
		test.use({ pageAuth });

		test('browserLogin leaves the page where it landed, with a session it minted', async ({ page }, testInfo) => {
			test.skip(testInfo.project.name !== 'desktop-chromium', 'the helper is viewport-independent; one project is enough');
			const sessionValue = async () => (await page.context().cookies()).find((c) => SESSION_COOKIE.test(c.name))?.value;
			const before = await sessionValue();
			const navigations: string[] = [];
			page.on('framenavigated', (f) => {
				if (f === page.mainFrame()) navigations.push(f.url());
			});

			await browserLogin(page);
			const landed = page.url();
			// Long enough for the app at /login to have moved itself (it did so
			// within a second on an idle machine); nothing on the landing document
			// can move it.
			await page.waitForTimeout(2_000);

			expect(navigations, "main-frame navigations: only the helper's own landing").toEqual([landed]);
			expect(page.url()).toBe(landed);
			// The helper's in-page login set the cookie: under 'session' the
			// context already had one, so it must be a different session.
			const after = await sessionValue();
			expect(after, 'the in-page login set a session cookie').toBeTruthy();
			expect(after, 'the session cookie is the one the in-page login minted').not.toBe(before);
		});
	});
}
