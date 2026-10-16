import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';

// BUG-3850 wiring (CONVE-19 shape: the tracker and provider are tested where
// they are built; this pins that ItemDetail binds them). Source-level, like
// signOutGuardWiring: it fails when a binding is removed or pointed elsewhere,
// not when the bound code misbehaves.
describe('BUG-3850: confirmed base wiring in ItemDetail', () => {
	const src = readFileSync('src/lib/components/items/ItemDetail.svelte', 'utf8');
	const provider = src.slice(src.indexOf('new CollabProvider(itemId, doc, {'));
	const tracker = src.slice(src.indexOf('const confirmedBase = createConfirmedBaseTracker({'));
	const trackerBody = tracker.slice(0, tracker.indexOf('\n\t});'));
	const snapshot = src.slice(src.indexOf('function crashDraftSnapshot()'));
	const snapshotBody = snapshot.slice(0, snapshot.indexOf('\n\t}\n'));

	it('finds the sites (vantage point)', () => {
		expect(provider.length).toBeLessThan(src.length);
		expect(trackerBody.length).toBeGreaterThan(0);
		expect(snapshotBody.length).toBeGreaterThan(0);
	});

	it('the provider refreshes the base on the confirming barrier ack', () => {
		const options = provider.slice(0, provider.indexOf('\n\t\t});'));
		expect(options).toContain('onConfirmed: () => confirmedBase.refresh(),');
	});

	it('the crash keeper refreshes it on every tick', () => {
		expect(src).toMatch(/registerCrashDraftKeeper\(\{[\s\S]*?observe: confirmedBase\.refresh,[\s\S]*?\}\);/);
	});

	it('the tracker reads the provider through providerConfirmed, never in raw mode', () => {
		expect(trackerBody).toContain('confirmed: () => providerConfirmed(collabProvider),');
		expect(trackerBody).toMatch(/context: \(\) => \{[\s\S]*!rawMode \? ctx : null;/);
	});

	it('the kept draft carries the confirmed snapshot and its seq', () => {
		expect(snapshotBody).toContain("return { markdown: md, baseSeq: confirmed.seq, baseBody: confirmed.markdown, ...(confirmed.remoteAfter ? { baseStale: true } : {}) };");
	});

	it('the tracker reads the remote-only counter, and restore goes through restoreEdits (lead ruling baa63dc3)', () => {
		expect(trackerBody).toContain('remoteVersion: () => collabProvider?.remoteDocVersion ?? 0,');
		const apply = src.slice(src.indexOf('async function applyRefusedDraftChanges('));
		expect(apply.slice(0, apply.indexOf('\n\t}\n'))).toContain('const edits = restoreEdits(d);');
	});

	it('the editor refreshes it before every local change (lead ruling c9db9612)', () => {
		expect(src).toMatch(/const ed = editorInstance;\s*if \(!ed\) return;\s*return registerRefreshBeforeLocalChange\(ed as never, confirmedBase\.refresh\);/);
	});

	it('the patch restore waits on restoreWaitsForEditor over the provider and the mounted editor, and the notice gets the reason (lead ruling 0337bf77)', () => {
		expect(src).toContain('restoreWaitsForEditor(collabProvider, !!editorInstance)');
		expect(src).toMatch(/let restoreBlocked = \$derived\([\s\S]*?!rawMode && draftAppliesAsPatch\(refusedDraft, item\?\.seq \?\? 0\) && restoreWaitsForEditor\(collabProvider, !!editorInstance\)[\s\S]*?\? RESTORE_WAITS_FOR_EDITOR[\s\S]*?: null,/);
		expect(src).toMatch(/if \(draftAppliesAsPatch\(d, item\.seq \?\? 0\)\) \{\s*if \(restoreBlocked\) return;\s*await applyRefusedDraftChanges\(d\);/);
		expect(src).toContain('restoreBlocked={restoreBlocked}');
	});
});
