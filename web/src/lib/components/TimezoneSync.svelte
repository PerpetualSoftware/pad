<script module lang="ts">
	// Shared across console/app-shell remounts, but never across users.
	const attempted = new Set<string>();
</script>

<script lang="ts">
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { tutorialsStore } from '$lib/tutorials/tutorials.svelte';
	import { browserZone, explicitTimezoneWrites, noteExplicitTimezoneWrite, sameZone } from '$lib/timezone';
	import type { UIDismissalKey } from '$lib/types';

	const browser = browserZone();
	let error = $state('');
	let saving = $state(false);
	const zone = $derived(authStore.user?.timezone);
	const key = $derived<UIDismissalKey | null>(browser && zone && !sameZone(zone, browser)
		? `tz-mismatch:${zone}|${browser}` : null);

	$effect(() => {
		const userId = authStore.userId;
		if (!userId) return;
		error = '';
		saving = false;
		if (!attempted.has(userId)) {
			attempted.add(userId);
			if (zone === null && browser) {
				const isSameIdentity = authStore.identityFence();
				const writesBefore = explicitTimezoneWrites();
				void api.auth.setTimezoneIfUnset(browser).then((res) => {
					// An explicit write that succeeded after this one began wins, whatever order the answers arrive in (TASK-4101 PR2a r1 H-L2).
					if (explicitTimezoneWrites() !== writesBefore) return;
					if (isSameIdentity() && res.timezone !== undefined) authStore.setTimezone(userId, res.timezone);
				}).catch(() => {}); // Unsupported browser zone: no retry this page load.
			}
		}
	});
	$effect(() => {
		if (key) void tutorialsStore.loadDismissed();
	});

	async function switchZone() {
		if (!browser) return;
		const userId = authStore.userId;
		const isSameIdentity = authStore.identityFence();
		saving = true;
		error = '';
		try {
			const res = await api.auth.setTimezone(browser);
			// Counted only once it has succeeded (TASK-4101 PR2a r2 H2-M2).
			noteExplicitTimezoneWrite();
			if (isSameIdentity() && res.timezone !== undefined) authStore.setTimezone(userId, res.timezone);
		} catch (err) {
			if (isSameIdentity()) error = err instanceof Error ? err.message : 'Failed to set time zone';
		} finally {
			if (isSameIdentity()) saving = false;
		}
	}
</script>

{#if key && tutorialsStore.dismissed && !tutorialsStore.dismissed.has(key)}
	<aside aria-label="Time zone mismatch">
		<p>Your time zone is {zone}. This browser uses {browser}.</p>
		<button onclick={switchZone} disabled={saving}>Switch</button>
		<button onclick={() => key && tutorialsStore.dismiss(key)} disabled={saving}>Keep</button>
		{#if error}<p role="alert">{error}</p>{/if}
	</aside>
{/if}

<style>
	aside {
		position: fixed; right: 1rem; bottom: 1rem; z-index: 100;
		max-width: min(24rem, calc(100vw - 2rem)); padding: 1rem;
		background: var(--bg-secondary); color: var(--text-primary);
		border: 1px solid var(--border); border-radius: 8px;
		box-shadow: 0 4px 16px #0002;
	}
	p { margin: 0 0 0.5rem; }
	button { margin-right: 0.5rem; }
</style>
