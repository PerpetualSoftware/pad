<script lang="ts">
	import { onDestroy, tick, untrack } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { confirmDialog } from '$lib/stores/confirmDialog.svelte';
	import DialogShell from '$lib/components/common/DialogShell.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import WorkspaceTile from './WorkspaceTile.svelte';
	import type { Workspace, WorkspaceGroup } from '$lib/types';

	let { groups, workspaces, mobile, initialEdit, onclose, onchanged }: {
		groups: WorkspaceGroup[];
		workspaces: Workspace[] | null;
		mobile: boolean;
		/** Open at the edit step of this group instead of the list. */
		initialEdit?: string;
		onclose: () => void;
		onchanged: (deletedId?: string) => Promise<void>;
	} = $props();
	const uid = $props.id();
	const namedGroups = $derived(groups.filter(group => group.kind === 'named'));
	let step = $state<'list' | 'edit'>('list');
	let editing = $state<string | null>(null);
	let name = $state('');
	let workspaceIds = $state<string[]>([]);
	let pending = $state(false);
	let error = $state('');
	let ticket = 0;
	onDestroy(() => { ticket++; });
	const selectedCount = $derived(workspaces?.filter(workspace => workspaceIds.includes(workspace.id)).length ?? 0);

	function edit(group?: WorkspaceGroup) {
		editing = group?.id ?? null; name = group?.name ?? ''; workspaceIds = [...(group?.workspace_ids ?? [])];
		error = ''; step = 'edit';
		// The button that opened the step leaves with the list, so focus moves to the first field.
		void tick().then(() => document.getElementById(`${uid}-name`)?.focus());
	}
	untrack(() => {
		const group = initialEdit ? namedGroups.find(candidate => candidate.id === initialEdit) : undefined;
		if (group) edit(group);
	});
	/** Back to the list, with focus on the group's Edit button, or on New group when there is none. */
	async function showList(focusGroup?: string | null) {
		step = 'list'; error = '';
		await tick();
		const controls = [...document.querySelectorAll<HTMLElement>(`[data-groups="${uid}"]`)];
		(controls.find(control => focusGroup && control.dataset.edit === focusGroup) ?? controls.find(control => 'new' in control.dataset))?.focus();
	}
	function workspaceLabel(workspace: Workspace) {
		const duplicate = workspaces!.filter(other => other.name === workspace.name).length > 1;
		return `${duplicate ? `${workspace.owner_username}/` : ''}${workspace.name}`;
	}
	function members(group: WorkspaceGroup) {
		if (!workspaces) return `${group.workspace_ids.length} workspace${group.workspace_ids.length === 1 ? '' : 's'}`;
		const names = workspaces.filter(workspace => group.workspace_ids.includes(workspace.id)).map(workspaceLabel);
		if (!names.length) return 'No workspaces';
		return `${names.slice(0, 3).join(', ')}${names.length > 3 ? ` +${names.length - 3}` : ''}`;
	}
	async function remove(group: WorkspaceGroup) {
		if (pending) return;
		const sameIdentity = authStore.identityFence();
		const confirmed = await confirmDialog.request({ title: `Delete ${group.name}?`, message: 'Its workspaces stay as they are. Only the group is removed.', confirmLabel: 'Delete group', danger: true });
		if (confirmed && sameIdentity()) await save(group.id);
	}
	async function save(deletedId?: string) {
		if (pending || (!deletedId && !name.trim())) return;
		const mine = ++ticket;
		const sameIdentity = authStore.identityFence();
		const current = () => mine === ticket && sameIdentity();
		pending = true; error = '';
		try {
			let focus = deletedId ? null : editing;
			if (deletedId) await api.workspaceGroups.remove(deletedId);
			// Without the workspace list there are no checkboxes, so a rename sends the name only and membership is untouched.
			else if (editing) await api.workspaceGroups.update(editing, workspaces ? { name: name.trim(), workspace_ids: [...workspaceIds] } : { name: name.trim() });
			else focus = (await api.workspaceGroups.create({ name: name.trim(), workspace_ids: [...workspaceIds] }))?.id ?? null;
			if (!current()) return;
			await onchanged(deletedId);
			if (!current()) return;
			// A created group is found by its id, or by its name when the answer carried none.
			if (!deletedId && !focus) focus = namedGroups.find(group => group.name === name.trim())?.id ?? null;
			pending = false; await showList(focus);
		} catch (failure) {
			if (!current()) return;
			const message = (failure as { message?: unknown })?.message;
			error = typeof message === 'string' && message ? message : 'Could not save the group. Try again.';
		} finally {
			if (current()) pending = false;
		}
	}
</script>

<DialogShell open {mobile} {pending} title="Workspace groups" maxWidth="560px" {onclose}>
	{#if step === 'list'}
		<p class="intro">A group is a saved set of workspaces. Pick one in the agenda to see only their commitments.</p>
		{#if error}<p role="alert">{error}</p>{/if}
		{#if namedGroups.length}
			<ul class="list">
				{#each namedGroups as group (group.id)}
					<li class="group" data-group={group.id}>
						<div class="names">
							<div class="name">{group.name}</div>
							<div class="members">{members(group)}</div>
						</div>
						<button type="button" class="icon" data-groups={uid} data-edit={group.id} aria-label={`Edit ${group.name}`} disabled={pending} onclick={() => edit(group)}>
							<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 20h4L19 9l-4-4L4 16v4z" /></svg>
						</button>
						<button type="button" class="icon" aria-label={`Delete ${group.name}`} disabled={pending} onclick={() => remove(group)}>
							<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" /></svg>
						</button>
					</li>
				{/each}
			</ul>
		{:else}<p class="intro">No groups yet.</p>{/if}
	{:else}
		<form id={`${uid}-form`} onsubmit={event => { event.preventDefault(); void save(); }}>
			<div class="step">
				<button type="button" class="icon" aria-label="Back to groups" disabled={pending} onclick={() => showList(editing)}>
					<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 6l-6 6 6 6" /></svg>
				</button>
				<h3>{editing ? 'Edit group' : 'New group'}</h3>
			</div>
			<div class="field">
				<label class="lab" for={`${uid}-name`}>Name</label>
				<input id={`${uid}-name`} type="text" required bind:value={name} disabled={pending} />
			</div>
			{#if workspaces}
				<fieldset disabled={pending}>
					<legend class="lab">Workspaces <span class="count">· <span>{selectedCount} of {workspaces.length} selected</span></span></legend>
					<div class="list">
						{#each workspaces as workspace (workspace.id)}
							<label class="check"><input type="checkbox" value={workspace.id} bind:group={workspaceIds} /><WorkspaceTile name={workspace.name} label={workspaceLabel(workspace)} /></label>
						{/each}
					</div>
				</fieldset>
			{:else}<p class="intro">Could not load workspaces.</p>{/if}
			{#if error}<p role="alert">{error}</p>{/if}
		</form>
	{/if}
	{#snippet footerStart()}
		{#if step === 'list'}
			<Button data-groups={uid} data-new disabled={pending} onclick={() => edit()}>
				<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>New group
			</Button>
		{:else if editing}
			{@const group = namedGroups.find(candidate => candidate.id === editing)}
			{#if group}<Button variant="danger" disabled={pending} onclick={() => remove(group)}>Delete group</Button>{/if}
		{/if}
	{/snippet}
	{#snippet footer()}
		{#if step === 'list'}
			<Button variant="primary" disabled={pending} onclick={() => { if (!pending) onclose(); }}>Done</Button>
		{:else}
			<Button disabled={pending} onclick={() => { if (!pending) void showList(editing); }}>Cancel</Button>
			<Button variant="primary" type="submit" form={`${uid}-form`} disabled={pending || !name.trim() || (!editing && workspaces === null)}>{editing ? 'Save group' : 'Create group'}</Button>
		{/if}
	{/snippet}
</DialogShell>

<style>
	form { display: flex; flex-direction: column; gap: var(--space-4); min-width: 0; }
	p { margin: 0; }
	.intro { font-size: 13px; color: var(--text-secondary); }
	.list { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; }
	.group { display: flex; align-items: center; gap: var(--space-2); min-height: 48px; padding: 6px 12px; }
	.group + .group, .check + .check { border-top: 1px solid var(--border-subtle); }
	.names { flex: 1; min-width: 0; overflow-wrap: anywhere; }
	.name { font-weight: 600; }
	.members { font-size: 12px; color: var(--text-muted); }
	.icon { display: grid; place-items: center; flex: 0 0 auto; width: 36px; height: 36px; padding: 0; border: 0; border-radius: var(--radius); background: transparent; color: var(--text-secondary); cursor: pointer; }
	.icon:hover:not(:disabled) { background: var(--bg-hover); color: var(--text-primary); }
	.icon:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 1px; }
	.icon:disabled { opacity: 0.5; cursor: default; }
	.step { display: flex; align-items: center; gap: var(--space-2); margin: calc(-1 * var(--space-2)) 0 calc(-1 * var(--space-2)) calc(-1 * var(--space-2)); }
	h3 { margin: 0; font-size: 14px; font-weight: 700; }
	.lab { display: block; margin-bottom: 6px; padding: 0; font-size: 12px; font-weight: 600; color: var(--text-secondary); }
	.count { font-weight: 400; color: var(--text-muted); }
	input[type='text'] { box-sizing: border-box; width: 100%; min-height: 36px; padding: 6px 10px; color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; font-size: 14px; }
	fieldset { min-width: 0; margin: 0; padding: 0; border: 0; }
	/* The name is the row's label here, so it reads as text, not as the muted cue it is in a row's meta line. */
	.check :global(.ws) { color: inherit; }
	.check { display: flex; align-items: center; gap: 10px; min-height: 44px; padding: 0 12px; overflow-wrap: anywhere; }
	@media (pointer: coarse) { .icon { width: 44px; height: 44px; } input[type='text'] { min-height: 44px; } }
</style>
