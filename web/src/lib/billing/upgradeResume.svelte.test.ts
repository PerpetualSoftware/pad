import { describe, it, expect, beforeEach } from 'vitest';
import { saveUpgradeResume, readUpgradeResume, clearUpgradeResume, UPGRADE_RESUME_TTL_MS } from './upgradeResume';

// TASK-3601: the blocked flow is remembered across checkout, briefly, once.
describe('upgradeResume', () => {
	beforeEach(() => clearUpgradeResume());

	it('round-trips a same-origin path and the blocked action', () => {
		saveUpgradeResume('/ada/ws/settings?tab=members', 'invite a member', 1000);
		expect(readUpgradeResume(2000)).toEqual({ path: '/ada/ws/settings?tab=members', action: 'invite a member', at: 1000 });
	});

	it('expires after an hour and is gone once expired', () => {
		saveUpgradeResume('/ada/ws', 'add an item', 0);
		expect(readUpgradeResume(UPGRADE_RESUME_TTL_MS + 1)).toBeNull();
		expect(readUpgradeResume(1)).toBeNull();
	});

	it('refuses anything but a same-origin path', () => {
		for (const p of ['//evil.example', 'https://evil.example', '/\\evil.example', 'ws']) {
			saveUpgradeResume(p, 'x', 0);
			expect(readUpgradeResume(1), p).toBeNull();
		}
	});

	it('is cleared once used', () => {
		saveUpgradeResume('/ada/ws', 'add an item', 0);
		clearUpgradeResume();
		expect(readUpgradeResume(1)).toBeNull();
	});
});
