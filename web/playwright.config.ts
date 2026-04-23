import { defineConfig, devices } from '@playwright/test';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Anchor all test paths to the config file's directory so runs are
// invariant under the caller's cwd (Playwright's default resolves
// relative paths against cwd, which differs between `npx playwright
// test` and CI invocations).
const HERE = dirname(fileURLToPath(import.meta.url));

/**
 * Playwright config for Pad end-to-end smoke tests.
 *
 * The suite exercises the real Pad binary (built from this repo) serving
 * its embedded web UI. That's the same shape a user gets from
 * `pad server start`, so regressions that only show up once the web
 * assets are embedded are caught here rather than in svelte-check.
 *
 * See e2e/global-setup.ts for how admin bootstrap + workspace seeding
 * happen before any test runs.
 */

const E2E_PORT = Number(process.env.PAD_E2E_PORT ?? 17800);
const E2E_HOST = process.env.PAD_E2E_HOST ?? '127.0.0.1';
const BASE_URL = `http://${E2E_HOST}:${E2E_PORT}`;

// Build a private data dir for this run so the test instance never
// touches the developer's real ~/.pad or another CI run's artifacts.
const DATA_DIR = process.env.PAD_E2E_DATA_DIR ?? resolve(HERE, '..', '.pad-e2e');

// Pad binary relative to the repo root. `make build` / `make build-go`
// writes ./pad in the repo root; CI builds it explicitly before the
// e2e job runs.
const PAD_BINARY = process.env.PAD_BINARY ?? resolve(HERE, '..', 'pad');


export default defineConfig({
	testDir: './e2e',
	timeout: 30_000,
	expect: { timeout: 5_000 },

	// Playwright's reporter list kept minimal; CI uses the list reporter
	// and uploads the HTML report as an artifact on failure.
	reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',

	// Run tests in parallel but cap workers on CI to keep the suite under
	// the 2-minute target quoted in TASK-689.
	fullyParallel: true,
	workers: process.env.CI ? 2 : undefined,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 1 : 0,

	use: {
		baseURL: BASE_URL,
		trace: 'retain-on-failure',
		video: 'retain-on-failure',
		screenshot: 'only-on-failure'
		// NOTE: auth is applied per-test via the fixture in e2e/fixtures.ts
		// rather than config.use.storageState. The config approach proved
		// flaky: Playwright resolves `storageState` against disk at
		// project-setup time (before globalSetup runs), so the file written
		// by globalSetup wasn't picked up. Passing cookies through an
		// explicit `test.extend` fixture sidesteps that ordering entirely.
	},

	globalSetup: './e2e/global-setup.ts',

	projects: [
		{
			name: 'desktop-chromium',
			use: { ...devices['Desktop Chrome'] }
		},
		{
			// Mobile viewport — triggers the mobile BottomSheet branch
			// (CONVE-639: isMobile ≤ 639.98px on WorkspaceSwitcher).
			// Pixel 7 ships with a Chromium defaultBrowserType, so we keep
			// one installed browser for both projects and avoid downloading
			// WebKit in CI.
			name: 'mobile-chromium',
			use: { ...devices['Pixel 7'] }
		}
	],

	webServer: {
		// Wipe the data dir BEFORE the server starts, so migrations run
		// against an empty SQLite file every run. Doing this in globalSetup
		// would race with the already-running server — it has the DB file
		// open by then.
		command: `rm -rf "${DATA_DIR}" && mkdir -p "${DATA_DIR}" && ${PAD_BINARY} server start`,
		url: `${BASE_URL}/api/v1/health`,
		timeout: 30_000,
		reuseExistingServer: !process.env.CI,
		stdout: 'pipe',
		stderr: 'pipe',
		env: {
			PAD_HOST: E2E_HOST,
			PAD_PORT: String(E2E_PORT),
			PAD_DATA_DIR: DATA_DIR,
			PAD_LOG_LEVEL: 'warn'
		}
	}
});
