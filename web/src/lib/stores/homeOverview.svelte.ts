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
	// A forced reload reuses its key, so only the latest load may publish.
	private gen = 0;
	private pending: Promise<void> | null = null;
	private inFlight: Promise<void> | null = null;
	constructor() {
		authStore.onIdentityChange(() => { this.key = ''; this.pending = null; this.inFlight = null; this.needs = null; this.agenda = null; this.error = ''; this.loading = false; });
	}
	get zone() { return authStore.user?.timezone || browserZone() || 'UTC'; }
	get today() { return todayIn(this.zone); }
	get monday() { const day = this.today; return shiftDay(day, -((new Date(day + 'T12:00Z').getUTCDay() + 6) % 7)); }
	private keyFor(path: string) { return `${authStore.userId}:${path}:${this.zone}:${this.today}`; }
	arrive(path: string): Promise<void> {
		if (this.keyFor(path) === this.key && this.inFlight) return this.inFlight;
		return this.load(path, true);
	}
	load(path: string, force = false): Promise<void> {
		const key = this.keyFor(path);
		if (!force && key === this.key) return this.pending ?? Promise.resolve();
		this.key = key;
		const gen = ++this.gen;
		const sameIdentity = authStore.identityFence();
		const from = dayWindow(this.monday, this.zone).from;
		const to = dayWindow(shiftDay(this.monday, 6), this.zone).to;
		this.loading = true;
		this.error = '';
		const pending = Promise.allSettled([Promise.resolve().then(() => api.needs.list({ sort: 'age', limit: 4 })), Promise.resolve().then(() => api.getMyAgenda(from, to, this.zone, undefined, ['completed'], 'omit'))]).then(([needs, agenda]) => {
			if (!sameIdentity() || this.key !== key || this.gen !== gen) return;
			if (needs.status === 'fulfilled') this.needs = needs.value;
			if (agenda.status === 'fulfilled') this.agenda = agenda.value;
			if (needs.status === 'rejected' || agenda.status === 'rejected') this.error = 'Could not load your overview. Try again.';
			this.loading = false;
		}).finally(() => {
			if (this.inFlight === pending) this.inFlight = null;
		});
		this.pending = pending;
		this.inFlight = pending;
		return pending;
	}
}
export const homeOverview = new HomeOverview();
