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

	it('the tracker reads the provider through providerConfirmed, never in raw mode, and pairs the row with the snapshot', () => {
		expect(trackerBody).toContain('confirmed: () => providerConfirmed(collabProvider),');
		expect(trackerBody).toMatch(/context: \(\) => \{[\s\S]*!rawMode \? ctx : null;/);
		expect(trackerBody).toContain("raw: () => item?.content ?? '',");
	});

	it('the kept draft carries the confirmed snapshot, its seq and its row', () => {
		expect(snapshotBody).toContain('return { markdown: md, baseSeq: confirmed.seq, baseBody: confirmed.markdown, rawBaseBody: confirmed.raw };');
	});

	it('the editor refreshes it before every local change (lead ruling c9db9612)', () => {
		expect(src).toMatch(/const ed = editorInstance;\s*if \(!ed\) return;\s*return registerRefreshBeforeLocalChange\(ed as never, confirmedBase\.refresh\);/);
	});

	it('restore allows the raw retry only for a row that is the same document as the base', () => {
		expect(src).toContain("const rawIsSameDocument = d.rawBaseBody !== undefined && collabFlusher.storageForm(d.rawBaseBody, wsSlug) === d.baseBody;");
		expect(src).toContain("planDraftRestore(d, item.content ?? '', rawIsSameDocument);");
	});
});
