<script lang="ts" module>
	import { api } from '$lib/api/client';

	/** Who a needs-you row can be given to (TASK-4643). */
	export interface GiveTarget {
		kind: 'role' | 'user';
		id: string;
		label: string;
	}

	export interface GiveTargets {
		roles: (GiveTarget & { icon: string })[];
		people: GiveTarget[];
		/** Set when that half failed to load; the other half still shows. */
		rolesError?: string;
		peopleError?: string;
	}

	/**
	 * The workspace's roles and members. Each half fails on its own: the members
	 * list needs workspace membership, so an item-grant guest with edit rights
	 * gets a 403 there and still sees the roles.
	 */
	export async function loadGiveTargets(ws: string): Promise<GiveTargets> {
		const [roles, members] = await Promise.allSettled([api.agentRoles.list(ws), api.members.list(ws)]);
		const out: GiveTargets = { roles: [], people: [] };
		if (roles.status === 'fulfilled') {
			out.roles = (roles.value ?? []).map((r) => ({ kind: 'role', id: r.id, label: r.name, icon: r.icon ?? '' }));
		} else {
			out.rolesError = 'Roles unavailable';
		}
		if (members.status === 'fulfilled') {
			out.people = (members.value?.members ?? []).map((m) => ({ kind: 'user', id: m.user_id, label: m.user_name || m.user_email }));
		} else {
			out.peopleError = 'Members unavailable';
		}
		return out;
	}
</script>

<script lang="ts">
	import { onMount } from 'svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { authStore } from '$lib/stores/auth.svelte';

	interface Props {
		ws: string;
		onpick: (target: GiveTarget) => void;
		/** Defaults to an uncached load; the page passes a cached one. */
		load?: (ws: string) => Promise<GiveTargets>;
	}

	let { ws, onpick, load = loadGiveTargets }: Props = $props();

	let targets = $state<GiveTargets | null>(null);

	// Loaded once on mount, not in an $effect: nothing here should re-run it.
	onMount(() => {
		let live = true;
		load(ws).then(
			(t) => {
				if (live) targets = t;
			},
			() => {
				if (live) targets = { roles: [], people: [], rolesError: 'Roles unavailable', peopleError: 'Members unavailable' };
			}
		);
		return () => {
			live = false;
		};
	});

	// The viewer is the one giving it away.
	let people = $derived((targets?.people ?? []).filter((p) => p.id !== authStore.userId));
	let empty = $derived(!!targets && targets.roles.length === 0 && people.length === 0 && !targets.rolesError && !targets.peopleError);
</script>

<div class="picker" aria-busy={!targets}>
	<p class="heading">Give to…</p>
	{#if !targets}
		<p class="note" role="status">Loading…</p>
	{:else if empty}
		<p class="note">Nobody else to give it to in this workspace.</p>
	{:else}
		{#if targets.roles.length > 0 || targets.rolesError}
			<p class="group-label">Roles</p>
			{#each targets.roles as r (r.id)}
				<MenuItem icon={r.icon || undefined} onclick={() => onpick({ kind: 'role', id: r.id, label: r.label })}>{r.label}</MenuItem>
			{/each}
			{#if targets.rolesError}<p class="note">{targets.rolesError}</p>{/if}
		{/if}
		{#if people.length > 0 || targets.peopleError}
			<p class="group-label">People</p>
			{#each people as p (p.id)}
				<MenuItem onclick={() => onpick({ kind: 'user', id: p.id, label: p.label })}>{p.label}</MenuItem>
			{/each}
			{#if targets.peopleError}<p class="note">{targets.peopleError}</p>{/if}
		{/if}
	{/if}
</div>

<style>
	.picker {
		display: flex;
		flex-direction: column;
		min-width: 220px;
	}
	.heading {
		margin: 0;
		padding: 4px 8px 6px;
		font-weight: 600;
		font-size: 0.85rem;
		color: var(--text-primary);
		border-bottom: 1px solid var(--border);
	}
	.group-label {
		margin: 6px 0 2px;
		padding: 0 8px;
		font-size: 0.7rem;
		font-weight: 600;
		letter-spacing: 0.04em;
		text-transform: uppercase;
		color: var(--text-muted);
	}
	.note {
		margin: 4px 0;
		padding: 0 8px;
		font-size: 0.8rem;
		color: var(--text-muted);
	}
</style>
