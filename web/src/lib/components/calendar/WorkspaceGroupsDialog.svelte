<script lang="ts">
	import { onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import type { Workspace, WorkspaceGroup } from '$lib/types';

	let { groups, workspaces, onclose, onchanged }: {
		groups: WorkspaceGroup[];
		workspaces: Workspace[] | null;
		onclose: () => void;
		onchanged: (deletedId?: string) => Promise<void>;
	} = $props();
	const headingId = $props.id();
	const namedGroups = $derived(groups.filter(group => group.kind === 'named'));
	let editing = $state<string | null>(null);
	let name = $state('');
	let workspaceIds = $state<string[]>([]);
	let confirming = $state<string | null>(null);
	let pending = $state(false);
	let error = $state('');
	let ticket = 0;
	onDestroy(() => { ticket++; });

	function edit(group?: WorkspaceGroup) {
		editing = group?.id ?? null; name = group?.name ?? ''; workspaceIds = [...(group?.workspace_ids ?? [])];
		confirming = null; error = '';
	}
	function workspaceLabel(workspace: Workspace) {
		const duplicate = workspaces!.filter(other => other.name === workspace.name).length > 1;
		return `${duplicate ? `${workspace.owner_username}/` : ''}${workspace.name}`;
	}
	async function save(deletedId?: string) {
		if (pending || (!deletedId && !name.trim())) return;
		const mine = ++ticket;
		const sameIdentity = authStore.identityFence();
		const current = () => mine === ticket && sameIdentity();
		pending = true; error = '';
		try {
			if (deletedId) await api.workspaceGroups.remove(deletedId);
			else {
				// Without the workspace list there are no checkboxes, so a rename sends the name only and membership is untouched.
				if (editing) await api.workspaceGroups.update(editing, workspaces ? { name: name.trim(), workspace_ids: [...workspaceIds] } : { name: name.trim() });
				else await api.workspaceGroups.create({ name: name.trim(), workspace_ids: [...workspaceIds] });
			}
			if (!current()) return;
			await onchanged(deletedId);
			if (current()) edit();
		} catch (failure) {
			if (!current()) return;
			const message = (failure as { message?: unknown })?.message;
			error = typeof message === 'string' && message ? message : 'Could not save the group. Try again.';
		} finally {
			if (current()) pending = false;
		}
	}
</script>

<Modal open={true} onclose={() => { if (!pending) onclose(); }} labelledby={headingId} maxWidth="560px">
	<div class="content">
		<h2 id={headingId}>Workspace groups</h2>
		{#if error}<p role="alert">{error}</p>{/if}
		<ul>
			{#each namedGroups as group (group.id)}
				<li>
					<span>{group.name}</span>
					<Button size="sm" disabled={pending} onclick={() => edit(group)}>Rename</Button>
					{#if confirming === group.id}
						<Button size="sm" disabled={pending} onclick={() => save(group.id)}>Confirm delete</Button>
						<Button size="sm" disabled={pending} onclick={() => confirming = null}>Cancel</Button>
					{:else}
						<Button size="sm" disabled={pending} onclick={() => confirming = group.id}>Delete</Button>
					{/if}
				</li>
			{/each}
		</ul>
		<form onsubmit={event => { event.preventDefault(); void save(); }}>
			<h3>{editing ? 'Edit group' : 'New group'}</h3>
			<label>Name <input required bind:value={name} disabled={pending} /></label>
			{#if workspaces}
				<fieldset disabled={pending}>
					<legend>Workspaces</legend>
					{#each workspaces as workspace (workspace.id)}
						<label><input type="checkbox" value={workspace.id} bind:group={workspaceIds} />{workspaceLabel(workspace)}</label>
					{/each}
				</fieldset>
			{:else}<p>Could not load workspaces.</p>{/if}
			<div class="actions">
				<Button type="submit" disabled={pending || !name.trim() || (!editing && workspaces === null)}>{editing ? 'Save group' : 'Create group'}</Button>
				{#if editing}<Button disabled={pending} onclick={() => edit()}>Cancel edit</Button>{/if}
			</div>
		</form>
		<Button disabled={pending} onclick={onclose}>Close</Button>
	</div>
</Modal>

<style>
	.content { padding: var(--space-5); overflow-y: auto; }
	h2 { margin-top: 0; }
	ul { list-style: none; padding: 0; }
	li, .actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); margin-bottom: var(--space-3); }
	li > span { flex: 1; overflow-wrap: anywhere; }
	label { display: flex; align-items: center; gap: var(--space-2); margin-block: var(--space-2); }
	input:not([type='checkbox']) { flex: 1; min-width: 0; padding: var(--space-2); color: var(--text-primary); background: var(--bg-primary); border: 1px solid var(--border); border-radius: var(--radius); font: inherit; }
	fieldset { margin-block: var(--space-3); border: 1px solid var(--border); }
	[role='alert'] { color: var(--text-danger, var(--text-primary)); }
</style>
