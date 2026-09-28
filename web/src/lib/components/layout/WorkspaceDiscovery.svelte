<script lang="ts">
	// The TopBar "+" discovery surface (PLAN-3002 U4 / TASK-3276, Dave's Q4
	// ruling): search the workspaces that are NOT open as tabs, create one,
	// and restore a recently deleted one. Since U3 the bar shows only the open
	// set, so this is how a workspace outside it is reached without going
	// through /console.
	//
	// A combobox over a listbox, not a Menu: focus stays in the search input
	// and the arrow keys move the active option (aria-activedescendant), which
	// the Menu primitive cannot do because it hands focus to its first row.
	import { tick } from 'svelte';
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { pushEscapeHandler, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { clickOutside } from '$lib/utils/clickOutside';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';
	import RecentlyDeletedWorkspaces from '$lib/components/layout/RecentlyDeletedWorkspaces.svelte';
	import PendingInvitations from '$lib/components/layout/PendingInvitations.svelte';
	import type { Workspace } from '$lib/types';

	interface Props {
		open: boolean;
		onclose: () => void;
		/** The "+" button: exempt from outside-press, and focus returns to it. */
		trigger?: HTMLElement;
	}

	let { open, onclose, trigger }: Props = $props();

	const LISTBOX_ID = 'workspace-discovery-options';

	let query = $state('');
	let activeIndex = $state(0);
	let inputEl: HTMLInputElement | undefined = $state(undefined);
	let panelEl: HTMLElement | undefined = $state(undefined);

	type Option = { kind: 'workspace'; ws: Workspace } | { kind: 'create' };

	// Every visible workspace that is not an open tab, filtered by name or
	// slug. The list is the workspace store's; the open set is the tabs
	// store's, so a workspace opened elsewhere drops out as its commit lands.
	let matches = $derived.by(() => {
		const openSlugs = new Set(tabsStore.tabs.map((t) => t.slug));
		const q = query.trim().toLowerCase();
		return workspaceStore.workspaces.filter(
			(ws) =>
				!openSlugs.has(ws.slug) &&
				(q === '' || ws.name.toLowerCase().includes(q) || ws.slug.toLowerCase().includes(q))
		);
	});

	let options = $derived<Option[]>([
		...matches.map((ws) => ({ kind: 'workspace' as const, ws })),
		{ kind: 'create' as const },
	]);

	// Keep the active option in range as the list narrows.
	$effect(() => {
		if (activeIndex >= options.length) activeIndex = Math.max(0, options.length - 1);
	});

	// Reset and focus the search each time the surface opens.
	$effect(() => {
		if (!open) return;
		query = '';
		activeIndex = 0;
		tick().then(() => inputEl?.focus());
	});

	$effect(() => {
		if (!open) return;
		return pushEscapeHandler(() => {
			close();
			return true;
		}, ESCAPE_PRIORITY.menu);
	});

	function close() {
		onclose();
		trigger?.focus();
	}

	function optionId(i: number): string {
		return `${LISTBOX_ID}-${i}`;
	}

	// Opening from search is an explicit choice, so the tab is KEPT (TASK-3276),
	// then you land on that workspace's last route. A failed open still
	// navigates: reaching the workspace is the point, the tab a convenience.
	async function openWorkspace(ws: Workspace) {
		onclose();
		const isSameIdentity = authStore.identityFence();
		await tabsStore.open(ws.slug).catch(() => {});
		if (!isSameIdentity()) return;
		goto(workspaceRestoreTarget(ws));
	}

	function createWorkspace() {
		onclose();
		uiStore.openCreateWorkspace();
	}

	function activate(option: Option | undefined) {
		if (!option) return;
		if (option.kind === 'workspace') void openWorkspace(option.ws);
		else createWorkspace();
	}

	function onInputKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			activeIndex = (activeIndex + 1) % options.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			activeIndex = (activeIndex - 1 + options.length) % options.length;
		} else if (e.key === 'Enter') {
			e.preventDefault();
			activate(options[activeIndex]);
		} else if (e.key === 'Escape') {
			// Handled here as well as on the escape stack, so a press in the
			// input closes this surface and nothing under it (Menu's pattern).
			e.preventDefault();
			e.stopPropagation();
			close();
		}
	}

	function onQueryInput() {
		activeIndex = 0;
	}
</script>

{#if open}
	<div
		class="discovery"
		bind:this={panelEl}
		use:clickOutside={{
			enabled: open,
			onOutside: onclose,
			extra: () => [trigger],
		}}
	>
		<input
			bind:this={inputEl}
			bind:value={query}
			oninput={onQueryInput}
			onkeydown={onInputKeydown}
			class="discovery-search"
			type="text"
			role="combobox"
			aria-label="Find a workspace"
			aria-expanded="true"
			aria-controls={LISTBOX_ID}
			aria-autocomplete="list"
			aria-activedescendant={options.length ? optionId(activeIndex) : undefined}
			placeholder="Find a workspace…"
			autocomplete="off"
			spellcheck="false"
		/>
		<ul id={LISTBOX_ID} class="discovery-options" role="listbox" aria-label="Workspaces not open">
			{#each options as option, i (option.kind === 'workspace' ? option.ws.slug : '__create')}
				<!-- Options are activated through the combobox's keys; a pointer
				     click is the mouse path to the same action. -->
				<!-- svelte-ignore a11y_click_events_have_key_events -->
				<li
					id={optionId(i)}
					role="option"
					aria-selected={i === activeIndex}
					class="discovery-option"
					class:active={i === activeIndex}
					class:create={option.kind === 'create'}
					data-ws-slug={option.kind === 'workspace' ? option.ws.slug : undefined}
					onmousedown={(e) => e.preventDefault()}
					onmousemove={() => (activeIndex = i)}
					onclick={() => activate(option)}
				>
					{#if option.kind === 'workspace'}
						<span class="discovery-name">{option.ws.name}</span>
						{#if option.ws.is_guest}<span class="discovery-meta">Shared with you</span>{/if}
					{:else}
						<span class="discovery-name">+ New workspace…</span>
					{/if}
				</li>
			{/each}
		</ul>
		{#if matches.length === 0}
			<p class="discovery-empty" aria-live="polite">
				{query.trim() ? 'No workspace outside your tabs matches.' : 'Every workspace is already open.'}
			</p>
		{/if}
		<PendingInvitations active={open} onaccepted={onclose} />
		<RecentlyDeletedWorkspaces active={open} />
	</div>
{/if}

<style>
	.discovery {
		position: absolute;
		top: calc(100% + 6px);
		left: 50%;
		transform: translateX(-50%);
		width: 300px;
		max-height: min(70vh, 480px);
		overflow-y: auto;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 30;
		padding-top: var(--space-2);
	}
	.discovery-search {
		display: block;
		width: calc(100% - 2 * var(--space-3));
		margin: 0 var(--space-3) var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.9em;
	}
	.discovery-search:focus {
		outline: none;
		border-color: var(--accent-blue);
	}
	.discovery-options {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.discovery-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-4);
		cursor: pointer;
		font-size: 0.9em;
		color: var(--text-primary);
	}
	.discovery-option.active {
		background: var(--bg-hover);
	}
	.discovery-option.create {
		color: var(--text-muted);
		border-top: 1px solid var(--border);
	}
	.discovery-name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.discovery-meta {
		flex-shrink: 0;
		color: var(--text-muted);
		font-size: 0.8em;
	}
	.discovery-empty {
		margin: 0;
		padding: var(--space-2) var(--space-4);
		color: var(--text-muted);
		font-size: 0.85em;
	}
</style>
