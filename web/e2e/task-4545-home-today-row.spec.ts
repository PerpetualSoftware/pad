import { test, expect } from './fixtures';
import type { Locator } from '@playwright/test';
import { authJson, createWorkspace, deleteWorkspace } from './lib/attachment-viewer';
import { browserLogin } from './lib/collab-helpers';

async function box(locator: Locator) {
	await expect(locator).toBeAttached();
	const bounds = await locator.boundingBox();
	expect(bounds).not.toBeNull();
	return bounds!;
}

for (const theme of ['light', 'dark'] as const) {
	test(`past-deadline row keeps its title at Home width (${theme})`, async ({ page, request, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop Home layout');
		test.setTimeout(60_000); // The real reminder tick runs every 30 seconds.
		await page.setViewportSize({ width: 1440, height: 900 });
		await page.addInitScript(value => localStorage.setItem('pad-theme', value), theme);
		const { slug: ws } = await createWorkspace(fixture, request, `Deadline row ${theme}`);
		const headers = authJson(fixture);
		try {
			const title = `Past deadline needs acknowledgement ${ws}`;
			const made = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { headers, data: { title } });
			expect(made.ok(), await made.text()).toBe(true);
			const item = await made.json();
			const ref = `${item.collection_prefix}-${item.item_number}`;
			// Keep the trigger in the future so creation can arm it, with target and
			// hard deadline already past. The lead makes the reminder due now.
			const trigger = new Date(Date.now() + 60_000);
			const created = await request.post(`/api/v1/workspaces/${ws}/items/${ref}/commitment`, {
				headers, data: {
					dtstart: trigger.toISOString().slice(0, 19), tz: 'UTC', audience: 'human', lead_minutes: 2,
					roles: { target_days: 0, target_secs: -7260, deadline_days: 0, deadline_secs: -3660, deadline_behavior: 'stays_owed' }
				}
			});
			expect(created.ok(), await created.text()).toBe(true);
			const from = new Date(Date.now() - 86400_000).toISOString();
			const to = new Date(Date.now() + 86400_000).toISOString();
			await expect.poll(async () => {
				const response = await request.get(`/api/v1/me/agenda?from=${from}&to=${to}&tz=UTC`, { headers });
				expect(response.ok(), await response.text()).toBe(true);
				const agenda = await response.json();
				return agenda.entries.find((entry: { workspace: string; item_ref: string }) => entry.workspace === ws && entry.item_ref === ref)?.fired;
			}, { timeout: 35_000, intervals: [500, 1000] }).toBe(true);

			await browserLogin(page);
			await page.goto('/');
			await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
			const homeRow = page.getByRole('region', { name: 'Today', exact: true }).locator('.agenda-row').filter({ hasText: title });
			await expect(homeRow.getByRole('button', { name: `Acknowledge ${ref}`, exact: true })).toBeVisible();
			await expect(homeRow.getByRole('button', { name: `Done ${ref}`, exact: true })).toBeVisible();
			await expect(homeRow.getByRole('button', { name: `Actions ${ref}`, exact: true })).toBeVisible();
			const titleBox = await box(homeRow.locator('.title a'));
			expect.soft(titleBox.width, 'Home title remains readable').toBeGreaterThanOrEqual(120);
			const chip = await box(homeRow.locator('[data-kind="deadline"]:visible'));
			const meta = await box(homeRow.locator('.meta'));
			for (const [label, other] of [['title', titleBox], ['meta', meta]] as const) {
				const intersects = chip.x < other.x + other.width && chip.x + chip.width > other.x && chip.y < other.y + other.height && chip.y + chip.height > other.y;
				expect.soft(intersects, `deadline chip does not overlap ${label}`).toBe(false);
			}
			expect.soft(chip.y, 'deadline is below the meta line').toBeGreaterThanOrEqual(meta.y + meta.height);
			const reference = homeRow.locator('.ref');
			const refBox = await box(reference);
			const lineHeight = await reference.evaluate(el => parseFloat(getComputedStyle(el).lineHeight));
			expect.soft(refBox.height, 'reference stays on one line').toBeLessThanOrEqual(lineHeight + 1);
			await testInfo.attach(`home-${theme}`, { body: await homeRow.screenshot(), contentType: 'image/png' });

			await page.goto('/agenda');
			const agendaRow = page.locator('.agenda-row').filter({ hasText: title }).filter({ has: page.getByRole('button', { name: `Done ${ref}`, exact: true }) }).first();
			const wideChip = await box(agendaRow.locator('[data-kind="deadline"]:visible'));
			const done = await box(agendaRow.getByRole('button', { name: `Done ${ref}`, exact: true }));
			expect(Math.abs(wideChip.y + wideChip.height / 2 - done.y - done.height / 2), 'full agenda keeps chip beside Done').toBeLessThanOrEqual(1);
			await testInfo.attach(`agenda-${theme}`, { body: await agendaRow.screenshot(), contentType: 'image/png' });
			// The same row responds to its list width without changing the viewport.
			await agendaRow.evaluate(el => { el.parentElement!.style.width = '450px'; });
			const narrowTitle = await box(agendaRow.locator('.title a'));
			const narrowChip = await box(agendaRow.locator('[data-kind="deadline"]:visible'));
			const actions = await box(agendaRow.locator('.actions'));
			expect(narrowTitle.width).toBeGreaterThanOrEqual(120);
			expect(actions.y).toBeGreaterThanOrEqual(narrowChip.y + narrowChip.height);
			const rowBox = await box(agendaRow);
			const paddingRight = await agendaRow.evaluate(el => parseFloat(getComputedStyle(el).paddingRight));
			expect(Math.abs(actions.x + actions.width - rowBox.x - rowBox.width + paddingRight), 'wrapped actions align right').toBeLessThanOrEqual(1);

			await agendaRow.evaluate(el => { el.parentElement!.style.removeProperty('width'); });
			await page.setViewportSize({ width: 390, height: 844 });
			await expect(agendaRow).toHaveClass(/compact/);
			await expect(agendaRow.locator('.meta [data-kind="deadline"]')).toBeVisible();
			await expect(agendaRow.getByRole('button', { name: `Acknowledge ${ref}`, exact: true })).toHaveCount(0);
			const phoneTitle = await box(agendaRow.locator('.title a'));
			const phoneChip = await box(agendaRow.locator('[data-kind="deadline"]'));
			expect(phoneChip.y).toBeGreaterThanOrEqual(phoneTitle.y + phoneTitle.height);
			await testInfo.attach(`phone-${theme}`, { body: await agendaRow.screenshot(), contentType: 'image/png' });
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
}
