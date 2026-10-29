import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import { api } from '$lib/api/client';
import { authStore } from '$lib/stores/auth.svelte';
let sequence = 0;
let currentA: string;
let currentB: string;

const reply = (data: unknown) => new Response(JSON.stringify(data), { headers: { 'Content-Type': 'application/json' } });
let zone: string | null | undefined;
let winner: string;
let dismissed: string[];
let patches: unknown[];
let dismissKeys: string[];
let patchReply: (() => Promise<Response>) | undefined;
let dismissalReply: (() => Promise<Response>) | undefined;
async function signIn(id = currentA, timezone = zone) {
	vi.spyOn(api.auth, 'session').mockResolvedValue({ authenticated: true, user: { id, timezone } } as never);
	await authStore.load();
	await tick();
}
async function mount(browser = 'Asia/Tokyo') {
	const { default: Sync } = await import('./TimezoneSync.svelte');
	vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValueOnce({ timeZone: browser } as never);
	return render(Sync);
}
beforeEach(async () => {
	currentA = `a-${++sequence}`; currentB = `b-${sequence}`;
	zone = null; winner = 'Europe/Paris'; dismissed = []; patches = []; dismissKeys = [];
	patchReply = undefined; dismissalReply = undefined;
	authStore.clear();
	vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
		if (init?.method === 'PATCH') {
			patches.push(JSON.parse(init.body as string));
			return patchReply ? patchReply() : reply({ timezone: winner });
		}
		if (init?.method === 'PUT') {
			dismissKeys.push(decodeURIComponent(url.split('/').pop()!));
			return reply({ dismissed: dismissKeys });
		}
		return dismissalReply ? dismissalReply() : reply({ dismissed });
	}));
	await signIn();
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });
it('default PATCH is once per user and adopts the response winner', async () => {
	const view = await mount();
	await waitFor(() => expect(authStore.user?.timezone).toBe(winner));
	expect(patches).toEqual([{ timezone: 'Asia/Tokyo', if_unset: true }]);
	view.unmount(); await mount();
	expect(patches).toHaveLength(1);
	await signIn(currentB, null);
	await waitFor(() => expect(patches).toHaveLength(2));
});
it('equal zone makes no PATCH and no card', async () => {
	await signIn(currentA, 'Europe/Paris'); await mount();
	await screen.findByRole('button', { name: 'Switch' });
	await signIn(currentA, 'Asia/Tokyo'); await tick();
	expect(patches).toEqual([]); expect(screen.queryByRole('button', { name: 'Switch' })).toBeNull();
});
it('different zones show Switch and Keep; aliases do not nag', async () => {
	await signIn(currentA, 'Europe/Paris'); const view = await mount();
	await screen.findByRole('button', { name: 'Switch' }); expect(screen.getByRole('button', { name: 'Keep' })).toBeInTheDocument();
	view.unmount(); await signIn(currentA, 'Asia/Calcutta'); await mount('Asia/Kolkata'); await tick();
	expect(screen.queryByRole('button', { name: 'Switch' })).toBeNull();
});
it('Keep dismisses the exact pair; a new browser pair shows again', async () => {
	await signIn(currentA, 'Europe/Paris'); const view = await mount();
	await fireEvent.click(await screen.findByRole('button', { name: 'Keep' }));
	await waitFor(() => expect(dismissKeys).toEqual(['tz-mismatch:Europe/Paris|Asia/Tokyo']));
	expect(screen.queryByRole('button', { name: 'Switch' })).toBeNull();
	view.unmount(); await mount('America/New_York'); await screen.findByRole('button', { name: 'Switch' });
});
it('Switch PATCH is unconditional and hides the card', async () => {
	await signIn(currentA, 'Europe/Paris'); winner = 'Asia/Tokyo'; await mount();
	await fireEvent.click(await screen.findByRole('button', { name: 'Switch' }));
	await waitFor(() => expect(authStore.user?.timezone).toBe('Asia/Tokyo'));
	expect(patches).toEqual([{ timezone: 'Asia/Tokyo' }]);
	expect(screen.queryByRole('button', { name: 'Switch' })).toBeNull();
});
it('pending dismissals render no card', async () => {
	let resolve!: (r: Response) => void;
	await signIn(currentA, 'Europe/Paris'); dismissalReply = () => new Promise((r) => { resolve = r; });
	await mount(); await waitFor(() => expect(resolve).toBeTypeOf('function'));
	expect(screen.queryByRole('button', { name: 'Switch' })).toBeNull();
	resolve(reply({ dismissed: [] })); await screen.findByRole('button', { name: 'Switch' });
});
it('late default response from A cannot write into B', async () => {
	let resolve!: (r: Response) => void;
	patchReply = () => new Promise((r) => { resolve = r; });
	await mount(); await waitFor(() => expect(patches).toHaveLength(1));
	await signIn(currentB, 'America/New_York');
	const response = reply({ timezone: 'Asia/Tokyo' }); const parsed = vi.spyOn(response, 'json');
	resolve(response); await waitFor(() => expect(parsed).toHaveBeenCalledOnce()); await tick();
	expect(authStore.user?.timezone).toBe('America/New_York');
	expect(JSON.stringify(authStore.session)).not.toContain('Asia/Tokyo');
});
it('setTimezone updates only a matching current id', () => {
	authStore.setTimezone(currentA, 'Europe/Paris'); expect(authStore.user?.timezone).toBe('Europe/Paris');
	authStore.setTimezone(currentB, 'Asia/Tokyo'); expect(authStore.user?.timezone).toBe('Europe/Paris');
	authStore.clear(); authStore.setTimezone(currentA, 'Asia/Tokyo'); expect(authStore.user).toBeNull();
});
it('account switch never reuses the previous users dismissals', async () => {
	await signIn(currentA, 'Europe/Paris'); await mount();
	await fireEvent.click(await screen.findByRole('button', { name: 'Keep' }));
	await signIn(currentB, 'Europe/Paris'); await screen.findByRole('button', { name: 'Switch' });
});
