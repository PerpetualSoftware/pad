import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import { TILE, VIEWER_IMAGE, itemUrl, uploadAttachment } from './lib/attachment-viewer';

/**
 * TASK-3319: the viewer says when a file was uploaded, and by whom, so a reader
 * can tell the order of a set of screenshots. The caption comes from the
 * `X-Pad-Attachment-Uploaded-At` / `-Uploaded-By` headers on the member route.
 */
test.describe('TASK-3319: the attachment viewer shows the upload time and uploader', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a caption check; one project is enough');
	});

	test('the caption reads "Uploaded <relative time> · by <name>", with the absolute time on hover', async ({
		page,
		fixture,
		request
	}) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, `Upload caption ${Date.now()}`);
		await uploadAttachment(fixture, request, doc.id, 'caption-shot.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		const caption = page.locator('.lightbox-meta-uploaded');
		await expect(caption).toBeVisible({ timeout: 10_000 });
		await expect(caption).toContainText(/^Uploaded .+ · by .+$/);
		// The absolute time is on hover, parseable as a date.
		const title = await caption.getAttribute('title');
		expect(title && !Number.isNaN(Date.parse(title)), `title "${title}" is not a date`).toBeTruthy();
	});
});
