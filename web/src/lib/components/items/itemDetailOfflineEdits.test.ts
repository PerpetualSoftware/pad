// Node-project test (no DOM): a SOURCE guard that ItemDetail wires TASK-2199's
// decisions (lib/collab/offlineEdits, tested on their own) into the places that
// act on them. Same reasoning as itemDetailIdentityDiscardsTeardownWrites.test.ts:
// ItemDetail is ~7,900 lines of collab and pane wiring. What a source guard
// cannot do: it checks the calls are there and in order, not that they run.
// The e2e task-2199-offline-close-asks.spec.ts drives the unload leg for real.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const SRC = readFileSync(resolve(__dirname, './ItemDetail.svelte'), 'utf8');
const CODE = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^[ \t]*\/\/.*$/gm, '');

function between(startMarker: string, endMarker: string): string {
	const start = CODE.indexOf(startMarker);
	if (start < 0) throw new Error(`${startMarker} was renamed or removed: re-point this guard`);
	const end = CODE.indexOf(endMarker, start);
	if (end <= start) throw new Error(`no ${endMarker} after ${startMarker}: re-point this guard`);
	return CODE.slice(start, end);
}

describe('TASK-2199: ItemDetail protects edits that have not reached the server', () => {
	it('the beforeunload prompt asks unloadLosesEdits with the unsent flag and the recovery', () => {
		const handler = between('const onBeforeUnload', "window.addEventListener('beforeunload'");
		expect(handler).toMatch(/unloadLosesEdits\(\{[\s\S]*unsentLocalEdits:\s*collabProvider\?\.editsAtRiskOnClose[\s\S]*recovery:\s*offlineRecovery[\s\S]*\}\)/); // BUG-3556
		expect(handler).toMatch(/unloadLosesEdits\([\s\S]*\)\s*\)\s*\{\s*event\.preventDefault\(\)/);
	});

	it('force_refresh captures what it discards BEFORE it tears the editor down', () => {
		const handler = between('onForceRefresh: () => {', 'void api.items');
		const capture = handler.indexOf('offlineRecoveryOnForceRefresh(');
		expect(capture, 'onForceRefresh does not capture the discarded edits').toBeGreaterThan(-1);
		// BUG-3526: an unconfirmed send counts too, not only an offline edit.
		expect(handler).toMatch(/unsentLocalEdits:\s*provider\.editsMayBeMissing/);
		expect(handler).toMatch(/liveMarkdown:\s*liveEditorMarkdown\(\)/);
		expect(capture, 'the capture runs after the teardown flags').toBeLessThan(handler.indexOf('skipFlushOnNextCleanup = true'));
		expect(handler).toMatch(/if \(recovered\) offlineRecovery = recovered/);
	});

	it('in-app navigation asks leaveQuestion and cancels on a No, leaving tab close to beforeunload', () => {
		const guard = between('beforeNavigate((nav) => {', '\n\t});');
		expect(guard).toMatch(/if \(nav\.type === 'leave'\) return;/);
		expect(guard).toMatch(/leaveQuestion\(\{[\s\S]*unsentLocalEdits:\s*collabProvider\?\.editsAtRiskOnClose[\s\S]*recovery:\s*offlineRecovery/); // BUG-3556
		expect(guard).toMatch(/if \(!confirm\(question\)\) \{\s*nav\.cancel\(\);/);
	});

	it('renders the recovery notice for this item, and copy or dismiss drops it', () => {
		expect(SRC).toMatch(/\{#if offlineRecovery && item && offlineRecovery\.itemId === item\.id\}[\s\S]{0,400}?<OfflineRecoveryNotice/);
		const notice = between('<OfflineRecoveryNotice', '/>');
		expect(notice).toMatch(/oncopied=\{\(copied\) => \{\s*if \(handedDown !== identityKey\) return;\s*if \(offlineRecovery\?\.text !== copied\) return;\s*offlineRecovery = null;/);
		expect(notice).toMatch(/ondismiss=\{\(\) => \(offlineRecovery = null\)\}/);
	});

	it('the offline tooltip no longer claims edits are saved locally', () => {
		expect(SRC).not.toMatch(/saved locally/i);
		expect(SRC).toMatch(/kept in this tab and will sync when the connection returns\. Closing the tab before then loses them\./);
	});
});
