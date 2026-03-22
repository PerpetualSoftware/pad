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
