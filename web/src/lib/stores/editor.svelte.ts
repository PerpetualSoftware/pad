// Module-singleton editor scalars — ONE `dirty`/`lastSaveTime`/`saveStatus`
// for the whole page, regardless of how many <ItemDetail> instances are
// mounted. That's fine today (only one is ever live), but PLAN-2154 (the
// full-page host + docked pane) will mount a second, concurrently-live
// instance. A scalar-consumer audit (PLAN-2154 D2 / R4, TASK-2156) found two
// consumer classes that must NOT read these singletons directly once that
// ships, because one instance's write would leak into the other's decision:
//   - ItemDetail's own SSE archive/delete guards — rerouted to a per-instance
//     `localDirty` $state shadow (see ItemDetail.svelte).
//   - ChildItems' self-save reload suppression — rerouted to `selfDirty`/
//     `selfLastSaveTime` props the owning ItemDetail passes down (see
//     ChildItems.svelte), instead of reading `editorStore.dirty`/
//     `lastSaveTime` here directly.
// `editorStore` itself is unchanged: it's still the source those instance-
// local shadows mirror on every write, and other single-per-page consumers
// (e.g. +layout.svelte's self-save suppression, keyed off the also-singleton
// `collectionStore.activeItem`) still read it directly — that's a separate,
// out-of-scope concern (PLAN-2154 R9).
let mode = $state<'edit' | 'raw'>('edit');
let saveStatus = $state<'idle' | 'saving' | 'saved' | 'error'>('idle');
let dirty = $state(false);
let lastSaveTime = $state(0);
let externalChange = $state(false);

export const editorStore = {
	get mode() { return mode; },
	get saveStatus() { return saveStatus; },
	get dirty() { return dirty; },
	get lastSaveTime() { return lastSaveTime; },
	get externalChange() { return externalChange; },

	setMode(m: 'edit' | 'raw') { mode = m; },
	setSaveStatus(s: 'idle' | 'saving' | 'saved' | 'error') { saveStatus = s; },
	setDirty(d: boolean) { dirty = d; },
	setLastSaveTime(t: number) { lastSaveTime = t; },
	setExternalChange(v: boolean) { externalChange = v; },

	enterRaw() { mode = 'raw'; },
	enterEdit() { mode = 'edit'; },
	resetForDoc() { mode = 'edit'; dirty = false; externalChange = false; },
};
