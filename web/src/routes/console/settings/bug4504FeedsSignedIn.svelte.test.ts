import { expect, it } from 'vitest';
import { readFileSync } from 'node:fs';

// BUG-4504: deleting the account calls authStore.clear(), which bumps the identity epoch. A
// feeds section keyed on that epoch alone remounted on the dead session, and its load's 401
// took the session-expiry redirect, aborting the delete flow's own navigation to /login. The
// section mounts only while signed in, so clear() unmounts it instead.
it('BUG-4504: the calendar feeds section mounts only while signed in', () => {
	const source = readFileSync('src/routes/console/settings/+page.svelte', 'utf8').replace(/<!--[\s\S]*?-->/g, '');
	const card = source.match(/<section class="card" id="calendar-feeds">[\s\S]*?<\/section>/)?.[0] ?? '';
	expect(card).toMatch(
		/\{#if authStore\.authenticated\}\s*\{#key authStore\.identityEpoch\}\s*<CalendarFeedsSettings \/>\s*\{\/key\}\s*\{\/if\}/
	);
});
