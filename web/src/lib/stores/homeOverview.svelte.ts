import { api } from '$lib/api/client';
import { authStore } from './auth.svelte';
import { dayWindow, shiftDay, todayIn } from '$lib/calendar/presentation';
import { browserZone } from '$lib/timezone';
import type { AgendaResponse, NeedsResponse } from '$lib/types';

class HomeOverview {
	needs = $state<NeedsResponse | null>(null);
	agenda = $state<AgendaResponse | null>(null);
	error = $state('');
	loading = $state(false);
	private key = '';
	private pending: Promise<void> | null = null;
	constructor() {
		authStore.onIdentityChange(() => { this.key = ''; this.pending = null; this.needs = null; this.agenda = null; this.error = ''; });
	}
	get zone() { return authStore.user?.timezone || browserZone() || 'UTC'; }
	get today() { return todayIn(this.zone); }
	get monday() { const day = this.today; return shiftDay(day, -((new Date(day + 'T12:00Z').getUTCDay() + 6) % 7)); }
	load(path: string, force = false): Promise<void> {
		const key = `${authStore.userId}:${path}:${this.today}`;
		if (!force && key === this.key) return this.pending ?? Promise.resolve();
		this.key = key;
		const sameIdentity = authStore.identityFence();
		const from = dayWindow(this.monday, this.zone).from;
		const to = dayWindow(shiftDay(this.monday, 6), this.zone).to;
		this.loading = true;
		this.error = '';
		this.pending = Promise.allSettled([Promise.resolve().then(() => api.needs.list({ sort: 'age', limit: 4 })), Promise.resolve().then(() => api.getMyAgenda(from, to, this.zone))]).then(([needs, agenda]) => {
			if (!sameIdentity() || this.key !== key) return;
			if (needs.status === 'fulfilled') this.needs = needs.value;
			if (agenda.status === 'fulfilled') this.agenda = agenda.value;
			if (needs.status === 'rejected' || agenda.status === 'rejected') this.error = 'Could not load your overview. Try again.';
			this.loading = false;
		});
		return this.pending;
	}
}
export const homeOverview = new HomeOverview();
