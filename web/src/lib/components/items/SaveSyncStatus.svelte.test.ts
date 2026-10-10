// TASK-2221 (audit C38): the sticky save/sync chip and its polite live region.
import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { SaveStatus } from '$lib/items/saveTracker.svelte';
import type { CollabConnectionState } from '$lib/collab/wsProvider.svelte';
import SaveSyncStatus from './SaveSyncStatus.svelte';

let app: Record<string, unknown> | null = null;
let host: HTMLElement;

function render(save: SaveStatus, collab: CollabConnectionState | null) {
	const props = $state({ saveStatus: save, collabState: collab });
	host = document.createElement('div');
	document.body.appendChild(host);
	app = mount(SaveSyncStatus, { target: host, props }) as Record<string, unknown>;
	flushSync();
	return props;
}

const chip = () => host.querySelector('.save-sync-chip')?.textContent ?? null;
const region = () => host.querySelector('[role="status"]')!;
const said = () => region().textContent!.replace(/ /g, '').trim();

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('SaveSyncStatus', () => {
	it('has a polite status region from the first render, before anything is said', () => {
		render('idle', 'connecting');
		expect(region().getAttribute('aria-live')).toBe('polite');
		expect(said()).toBe('');
		expect(chip()).toBeNull();
	});

	it('shows saving and saved, and announces only the save', () => {
		const p = render('idle', 'synced');
		p.saveStatus = 'saving';
		flushSync();
		expect(chip()).toBe('Saving…');
		expect(said()).toBe('');
		p.saveStatus = 'saved';
		flushSync();
		expect(chip()).toBe('✓ Saved');
		expect(said()).toBe('Saved');
		p.saveStatus = 'idle';
		flushSync();
		expect(chip()).toBeNull();
	});

	it('a second save is a change to the region, so it is announced again', () => {
		const p = render('idle', null);
		p.saveStatus = 'saved';
		flushSync();
		const first = region().textContent;
		p.saveStatus = 'idle';
		flushSync();
		p.saveStatus = 'saved';
		flushSync();
		expect(region().textContent).not.toBe(first);
		expect(said()).toBe('Saved');
	});

	it('announces offline, reconnecting and back online; the first connect says nothing', () => {
		const p = render('idle', 'connecting');
		p.collabState = 'synced';
		flushSync();
		expect(said()).toBe('');
		p.collabState = 'offline';
		flushSync();
		expect(chip()).toBe('Offline');
		expect(said()).toMatch(/^Offline/);
		p.collabState = 'reconnecting';
		flushSync();
		expect(chip()).toBe('Reconnecting…');
		expect(said()).toMatch(/Reconnecting/);
		p.collabState = 'synced';
		flushSync();
		expect(chip()).toBeNull();
		expect(said()).toMatch(/^Back online/);
	});

	it('a connection problem outranks a save in the chip', () => {
		render('saving', 'offline');
		expect(chip()).toBe('Offline');
	});
});
