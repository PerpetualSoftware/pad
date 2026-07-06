import { test, expect } from './fixtures';

/**
 * Danger Zone — "Export my data" (TASK-1961 / TASK-1963).
 *
 * Uses the shared authenticated fixture (admin Bearer header applied to the
 * context). The export endpoint is read-only, so exercising it as the suite
 * admin is safe and touches no other spec's state.
 *
 * The client (web/src/lib/utils/artifacts.ts::exportAndDownloadAccountData)
 * GETs /api/v1/auth/export, then triggers a Blob download named from the
 * server's Content-Disposition. We assert the download fires with the
 * contract filename and that the success line renders — proving the client
 * saw a 200 and parsed the artifact. The exact header + JSON shape are pinned
 * server-side in internal/server/handlers_account_export_contract_test.go.
 */
test('Danger Zone: Export my data downloads pad-export.json', async ({ page }) => {
	await page.goto('/console/settings');

	const exportBtn = page.getByRole('button', { name: 'Export my data', exact: true });
	await expect(exportBtn).toBeVisible();

	const [download] = await Promise.all([page.waitForEvent('download'), exportBtn.click()]);

	// The filename the browser saves under comes straight from the server's
	// `Content-Disposition: attachment; filename="pad-export.json"`.
	expect(download.suggestedFilename()).toBe('pad-export.json');

	await expect(page.getByText('Your data export has downloaded.')).toBeVisible();
});
