import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * The e2e server's data dir: PAD_E2E_DATA_DIR, else `.pad-e2e` at the REPO
 * root. playwright.config.ts hands it to the server and specs that read the
 * server's SQLite file take it from here, so the two cannot drift (BUG-3523:
 * a spec that resolved it one level too shallow passed locally, where the
 * variable was set, and could not open the file in CI).
 */
export const E2E_DATA_DIR =
	process.env.PAD_E2E_DATA_DIR ?? resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '.pad-e2e');

/** The e2e server's SQLite file. */
export const E2E_DB_PATH = resolve(E2E_DATA_DIR, 'pad.db');
