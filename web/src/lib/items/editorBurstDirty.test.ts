// TASK-2232: an editor burst's dirty marks. See editorBurstDirty.ts.
import { describe, it, expect } from 'vitest';
import { createBurstDirty } from './editorBurstDirty';

function harness(start: { local?: boolean; store?: boolean; owns?: boolean } = {}) {
	const s = { local: start.local ?? false, store: start.store ?? false, owns: start.owns ?? true };
	const d = createBurstDirty({
		localDirty: () => s.local,
		setLocalDirty: (v) => { s.local = v; },
		ownsStore: () => s.owns,
		storeDirty: () => s.store,
		setStoreDirty: (v) => { s.store = v; },
	});
	return { s, d };
}

describe('an editor burst\'s dirty marks (TASK-2232)', () => {
	it('a change marks both at once', () => {
		const { s, d } = harness();
		d.changed();
		expect(s).toMatchObject({ local: true, store: true });
	});

	it('a burst that settles unchanged puts back the marks it raised', () => {
		const { s, d } = harness();
		d.changed();
		d.changed();
		d.settledUnchanged();
		expect(s).toMatchObject({ local: false, store: false });
	});

	it('marks an earlier real change set stay: they are owed a save', () => {
		const { s, d } = harness({ local: true, store: true });
		d.changed();
		d.settledUnchanged();
		expect(s).toMatchObject({ local: true, store: true });
	});

	it('a delivered burst keeps its marks, even if a later burst settles unchanged', () => {
		const { s, d } = harness();
		d.changed();
		d.delivered();
		d.changed();
		d.settledUnchanged();
		expect(s).toMatchObject({ local: true, store: true });
	});

	it('a side that does not own the singleton never touches it', () => {
		const { s, d } = harness({ owns: false });
		d.changed();
		expect(s).toMatchObject({ local: true, store: false });
		s.store = true; // the active side set it meanwhile
		d.settledUnchanged();
		expect(s).toMatchObject({ local: false, store: true });
	});
});
