<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import type { Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import CreateCollectionModal from '$lib/components/collections/CreateCollectionModal.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';

	let wsSlug = $derived(page.params.workspace ?? '');
	let loading = $state(true);
	let collections = $state<Collection[]>([]);
	let wsName = $state('');
	let savingName = $state(false);
	let nameStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let theme = $state<'light' | 'dark'>('dark');
	let showCreateModal = $state(false);
	let editingCollection = $state<Collection | null>(null);

	// Members
	let members = $state<{ user_id: string; user_name: string; user_email: string; role: string }[]>([]);
	let invitations = $state<{ id: string; email: string; role: string; code: string; join_url?: string }[]>([]);
	let inviteEmail = $state('');
	let inviteRole = $state('editor');
	let inviting = $state(false);
	let inviteResult = $state<{ message: string; type: 'success' | 'error' } | null>(null);
	let currentUserRole = $state('');

	// Tabs
	let activeTab = $state('general');
	const tabs = [
		{ id: 'general', label: 'General', icon: '\u2699\uFE0F' },
		{ id: 'members', label: 'Members', icon: '\uD83D\uDC65' },
		{ id: 'collections', label: 'Collections', icon: '\uD83D\uDCC1' },
		{ id: 'danger', label: 'Danger Zone', icon: '\u26A0\uFE0F' },
	];
	const validTabIds = tabs.map(t => t.id);

	function switchTab(tabId: string) {
		activeTab = tabId;
		history.replaceState(null, '', `#${tabId}`);
	}

	$effect(() => {
		if (wsSlug) load(wsSlug);
	});

	onMount(() => {
		const stored = localStorage.getItem('pad-theme');
		if (stored === 'light' || stored === 'dark') {
			theme = stored;
		} else {
			theme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
		}
		const hash = window.location.hash.slice(1);
		if (validTabIds.includes(hash)) activeTab = hash;
	});
	async function load(slug: string) {
		loading = true;
		try {
			await workspaceStore.setCurrent(slug);
			wsName = workspaceStore.current?.name ?? '';
			collections = await api.collections.list(slug);
			try {
				const memberData = await api.members.list(slug);
				members = memberData.members ?? [];
				invitations = memberData.invitations ?? [];
				// Determine current user's role
				const session = await api.auth.session();
				if (session.user) {
					const me = members.find(m => m.user_email === session.user!.email);
					currentUserRole = me?.role ?? '';
				}
			} catch {}
		} catch { /* allow partial render */
		} finally {
			loading = false;
		}
	}
	async function saveName() {
		if (!wsName.trim() || savingName) return;
		savingName = true;
		nameStatus = 'idle';
		try {
			await api.workspaces.update(wsSlug, { name: wsName.trim() });
			nameStatus = 'saved';
			setTimeout(() => (nameStatus = 'idle'), 2000);
		} catch {
			nameStatus = 'error';
		} finally {
			savingName = false;
		}
	}
	function toggleTheme() {
		theme = theme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', theme);
		localStorage.setItem('pad-theme', theme);
	}
	async function handleCollectionCreated() {
		collections = await api.collections.list(wsSlug);
		collectionStore.loadCollections(wsSlug);
		showCreateModal = false;
	}
	async function handleCollectionUpdated() {
		collections = await api.collections.list(wsSlug);
		collectionStore.loadCollections(wsSlug);
		editingCollection = null;
	}
	async function handleInvite() {
		if (!inviteEmail.trim() || inviting) return;
		inviting = true;
		inviteResult = null;
		try {
			const result = await api.members.invite(wsSlug, inviteEmail.trim(), inviteRole);
			if (result.added) {
				inviteResult = { message: `Added ${result.name || result.email} as ${result.role}`, type: 'success' };
			} else if (result.join_url) {
				inviteResult = { message: `Invitation sent to ${result.email}. Link copied to clipboard!`, type: 'success' };
				navigator.clipboard.writeText(result.join_url).catch(() => {});
			} else {
				inviteResult = { message: `Invitation sent to ${result.email}. Join code: ${result.code}`, type: 'success' };
			}
			inviteEmail = '';
			inviteRole = 'editor';
			// Reload members
			const memberData = await api.members.list(wsSlug);
			members = memberData.members ?? [];
			invitations = memberData.invitations ?? [];
		} catch (err: unknown) {
			inviteResult = { message: err instanceof Error ? err.message : 'Failed to invite', type: 'error' };
		} finally {
			inviting = false;
		}
	}

	async function handleRemoveMember(userId: string, name: string) {
		if (!confirm(`Remove ${name} from this workspace?`)) return;
		try {
			await api.members.remove(wsSlug, userId);
			members = members.filter(m => m.user_id !== userId);
			toastStore.show(`Removed ${name}`, 'success');
		} catch {
			toastStore.show('Failed to remove member', 'error');
		}
	}

	async function handleChangeRole(userId: string, newRole: string) {
		try {
			await api.members.updateRole(wsSlug, userId, newRole);
			members = members.map(m => m.user_id === userId ? { ...m, role: newRole } : m);
			toastStore.show('Role updated', 'success');
		} catch {
			toastStore.show('Failed to update role', 'error');
		}
	}

	let isOwner = $derived(currentUserRole === 'owner');

	let confirmDelete = $state(false);
	let deleting = $state(false);
	let deleteInput = $state('');

	async function handleDeleteWorkspace() {
		if (deleteInput !== wsSlug) return;
		deleting = true;
		try {
			await api.workspaces.delete(wsSlug);
			toastStore.show(`Workspace "${wsName}" archived`, 'success');
			goto('/');
		} catch {
			toastStore.show('Failed to archive workspace', 'error');
			deleting = false;
		}
	}

	let createdDate = $derived(
		workspaceStore.current?.created_at
			? new Date(workspaceStore.current.created_at).toLocaleDateString('en-US', {
					year: 'numeric',
					month: 'long',
					day: 'numeric'
				})
			: ''
	);
</script>

<div class="settings">
	{#if loading}
		<div class="loading">Loading settings...</div>
	{:else}
		<header class="settings-header">
			<h1>Settings</h1>
		</header>

		<div class="tab-bar" role="tablist">
			{#each tabs as tab (tab.id)}
				<button
					class="tab"
					class:active={activeTab === tab.id}
					class:danger={tab.id === 'danger'}
					role="tab"
					aria-selected={activeTab === tab.id}
					onclick={() => switchTab(tab.id)}
				>
					<span class="tab-icon">{tab.icon}</span>
					{tab.label}
				</button>
			{/each}
		</div>

		{#if activeTab === 'general'}
			<section class="section">
				<h2>Workspace</h2>
				<div class="card">
					<div class="field-row">
						<label for="ws-name">Name</label>
						<div class="inline-edit">
							<input id="ws-name" type="text" bind:value={wsName} onkeydown={(e) => e.key === 'Enter' && saveName()} />
							<button class="btn btn-small" onclick={saveName} disabled={savingName}>
								{savingName ? 'Saving...' : 'Save'}
							</button>
							{#if nameStatus === 'saved'}
								<span class="status-saved">Saved</span>
							{:else if nameStatus === 'error'}
								<span class="status-error">Error</span>
							{/if}
						</div>
					</div>
					<div class="field-row">
						<span class="field-label">Slug</span>
						<span class="field-value mono">{wsSlug}</span>
					</div>
					{#if createdDate}
						<div class="field-row">
							<span class="field-label">Created</span>
							<span class="field-value">{createdDate}</span>
						</div>
					{/if}
					<div class="field-row">
						<span class="field-label">Export</span>
						<a href="/api/v1/workspaces/{wsSlug}/export" download="{wsSlug}-export.json" class="btn btn-small">
							Download JSON
						</a>
					</div>
				</div>
			</section>
			<section class="section">
				<h2>Theme</h2>
				<div class="card">
					<div class="theme-row">
						<span>Appearance</span>
						<button class="theme-toggle" onclick={toggleTheme}>
							<span class="theme-option" class:active={theme === 'light'}>Light</span>
							<span class="theme-option" class:active={theme === 'dark'}>Dark</span>
						</button>
					</div>
				</div>
			</section>
		{:else if activeTab === 'members'}
			<section class="section">
				{#if members.length === 0}
					<p class="empty-text">No members yet.</p>
				{:else}
					<div class="members-list">
						{#each members as member (member.user_id)}
							<div class="card member-row">
								<div class="member-info">
									<span class="member-avatar">{member.user_name.charAt(0).toUpperCase()}</span>
									<div class="member-details">
										<span class="member-name">{member.user_name}</span>
										<span class="member-email">{member.user_email}</span>
									</div>
								</div>
								<div class="member-actions">
									{#if isOwner}
										<select
											class="role-select"
											value={member.role}
											onchange={(e) => handleChangeRole(member.user_id, (e.target as HTMLSelectElement).value)}
										>
											<option value="owner">Owner</option>
											<option value="editor">Editor</option>
											<option value="viewer">Viewer</option>
										</select>
										<button class="btn btn-small btn-remove" onclick={() => handleRemoveMember(member.user_id, member.user_name)}>
											Remove
										</button>
									{:else}
										<span class="role-badge">{member.role}</span>
									{/if}
								</div>
							</div>
						{/each}
					</div>
				{/if}

				{#if invitations.length > 0}
					<div class="invitations-section">
						<h3>Pending Invitations</h3>
						{#each invitations as inv (inv.id)}
							<div class="card invitation-row">
								<span class="inv-email">{inv.email}</span>
								<span class="role-badge">{inv.role}</span>
								{#if inv.join_url}
									<button class="btn btn-small copy-link-btn" onclick={() => { navigator.clipboard.writeText(inv.join_url ?? ''); toastStore.show('Link copied!', 'success'); }}>
										Copy invite link
									</button>
								{:else}
									<code class="inv-code">{inv.code}</code>
								{/if}
							</div>
						{/each}
					</div>
				{/if}

				{#if isOwner}
					<div class="invite-form card">
						<h3>Invite Member</h3>
						<div class="invite-row">
							<input
								type="email"
								placeholder="Email address"
								bind:value={inviteEmail}
								onkeydown={(e) => e.key === 'Enter' && handleInvite()}
								disabled={inviting}
							/>
							<select class="role-select" bind:value={inviteRole}>
								<option value="editor">Editor</option>
								<option value="viewer">Viewer</option>
								<option value="owner">Owner</option>
							</select>
							<button class="btn btn-primary btn-small" onclick={handleInvite} disabled={inviting || !inviteEmail.trim()}>
								{inviting ? 'Inviting...' : 'Invite'}
							</button>
						</div>
						{#if inviteResult}
							<p class="invite-result" class:invite-success={inviteResult.type === 'success'} class:invite-error={inviteResult.type === 'error'}>
								{inviteResult.message}
							</p>
						{/if}
					</div>
				{/if}
			</section>
		{:else if activeTab === 'collections'}
			<section class="section">
				{#if collections.length === 0}
					<p class="empty-text">No collections yet.</p>
				{:else}
					<div class="coll-list">
						{#each collections as coll (coll.id)}
							{@const schema = parseSchema(coll)}
							<button class="card coll-card coll-card-btn" onclick={() => (editingCollection = coll)}>
								<div class="coll-header">
									<span class="coll-icon">{coll.icon || '#'}</span>
									<span class="coll-name">{coll.name}</span>
									<span class="coll-slug mono">/{coll.slug}</span>
									<span class="coll-count">{coll.item_count ?? 0} items</span>
									{#if coll.is_default}
										<span class="badge">default</span>
									{/if}
									<span class="edit-hint">Edit</span>
								</div>
								{#if schema.fields.length > 0}
									<div class="field-tags">
										{#each schema.fields as field (field.key)}
											<span class="field-tag">{field.key}: {field.type}</span>
										{/each}
									</div>
								{/if}
							</button>
						{/each}
					</div>
				{/if}
				<button class="btn btn-create" onclick={() => (showCreateModal = true)}>
					+ Create Collection
				</button>
				<CreateCollectionModal
					open={showCreateModal}
					{wsSlug}
					oncreated={handleCollectionCreated}
					onclose={() => (showCreateModal = false)}
				/>
				{#if editingCollection}
					<EditCollectionModal
						open={true}
						collection={editingCollection}
						{wsSlug}
						onupdated={handleCollectionUpdated}
						onclose={() => (editingCollection = null)}
					/>
				{/if}
			</section>
		{:else if activeTab === 'danger'}
			<section class="section">
				<div class="danger-banner">
					<p>These actions are destructive and cannot be easily reversed.</p>
				</div>
				<div class="card danger-card">
					{#if !confirmDelete}
						<div class="danger-row">
							<div class="danger-info">
								<strong>Archive this workspace</strong>
								<p>This will hide the workspace and all its collections, items, and documents. The data is preserved but no longer accessible.</p>
							</div>
							<button class="btn btn-danger" onclick={() => confirmDelete = true}>
								Archive workspace
							</button>
						</div>
					{:else}
						<div class="danger-confirm">
							<p class="danger-warning">This will archive <strong>{wsName}</strong> and all its contents. To confirm, type the workspace slug below:</p>
							<div class="danger-input-row">
								<code class="slug-hint">{wsSlug}</code>
								<input
									type="text"
									class="danger-input"
									bind:value={deleteInput}
									placeholder="Type workspace slug to confirm"
									onkeydown={(e) => e.key === 'Enter' && handleDeleteWorkspace()}
								/>
							</div>
							<div class="danger-actions">
								<button class="btn btn-danger" onclick={handleDeleteWorkspace} disabled={deleteInput !== wsSlug || deleting}>
									{deleting ? 'Archiving...' : 'Archive this workspace'}
								</button>
								<button class="btn" onclick={() => { confirmDelete = false; deleteInput = ''; }}>Cancel</button>
							</div>
						</div>
					{/if}
				</div>
			</section>
		{/if}
	{/if}
</div>

<style>
	.settings { max-width: var(--content-max-width); margin: 0 auto; padding: var(--space-8) var(--space-6); }
	.loading { text-align: center; padding-top: 20vh; color: var(--text-muted); }
	.settings-header { margin-bottom: var(--space-4); }
	.settings-header h1 { font-size: 1.6em; }
	/* ── Tab bar ──── */
	.tab-bar {
		display: flex;
		gap: var(--space-1);
		border-bottom: 1px solid var(--border);
		margin-bottom: var(--space-6);
		overflow-x: auto;
		scrollbar-width: none;
		-webkit-overflow-scrolling: touch;
	}
	.tab-bar::-webkit-scrollbar { display: none; }
	.tab {
		padding: var(--space-2) var(--space-4);
		font-size: 0.9em;
		color: var(--text-secondary);
		cursor: pointer;
		border: none;
		background: none;
		border-bottom: 2px solid transparent;
		white-space: nowrap;
		transition: color 0.15s, border-color 0.15s;
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.tab:hover { color: var(--text-primary); }
	.tab.active {
		color: var(--text-primary);
		border-bottom-color: var(--accent-blue);
		font-weight: 500;
	}
	.tab-icon { font-size: 0.9em; }
	.tab.danger { color: var(--text-muted); }
	.tab.danger:hover { color: #ef4444; }
	.tab.danger.active { color: #ef4444; border-bottom-color: #ef4444; }
	/* ── Sections ──── */
	.section { margin-bottom: var(--space-8); }
	.section h2 { font-size: 1.1em; color: var(--text-secondary); margin-bottom: var(--space-4); }
	.card { background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius); padding: var(--space-4); }
	.card + .card { margin-top: var(--space-3); }
	.field-row { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-2) 0; flex-wrap: wrap; }
	.field-row + .field-row { border-top: 1px solid var(--border-subtle); }
	.field-row label, .field-label { width: 80px; font-size: 0.85em; color: var(--text-secondary); flex-shrink: 0; }
	.field-value { font-size: 0.9em; }
	.mono { font-family: var(--font-mono); font-size: 0.85em; }
	.inline-edit { display: flex; align-items: center; gap: var(--space-2); flex: 1; min-width: 0; flex-wrap: wrap; }
	.inline-edit input { flex: 1; min-width: 120px; max-width: 300px; }
	.status-saved { font-size: 0.8em; color: var(--accent-green); }
	.status-error { font-size: 0.8em; color: var(--accent-orange); }
	.btn { padding: var(--space-2) var(--space-4); background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); font-size: 0.85em; cursor: pointer; color: var(--text-primary); }
	.btn:hover { background: var(--bg-hover); }
	.btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.btn-small { padding: var(--space-1) var(--space-3); font-size: 0.8em; }
	.btn-primary { background: var(--accent-blue); border-color: var(--accent-blue); color: #fff; }
	.btn-primary:hover { opacity: 0.9; }
	.btn-create { margin-top: var(--space-3); width: 100%; padding: var(--space-3); background: var(--bg-secondary); border: 1px dashed var(--border); border-radius: var(--radius); color: var(--text-secondary); font-size: 0.85em; cursor: pointer; }
	.btn-create:hover { border-color: var(--accent-blue); color: var(--accent-blue); }
	.coll-list { display: flex; flex-direction: column; gap: var(--space-3); }
	.coll-card { padding: var(--space-3) var(--space-4); }
	.coll-header { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
	.coll-icon { font-size: 1.1em; }
	.coll-name { font-weight: 600; font-size: 0.95em; }
	.coll-slug { color: var(--text-muted); font-size: 0.8em; }
	.coll-card-btn { cursor: pointer; text-align: left; width: 100%; transition: border-color 0.15s; }
	.coll-card-btn:hover { border-color: var(--accent-blue); }
	.edit-hint { font-size: 0.75em; color: var(--text-muted); opacity: 0; transition: opacity 0.15s; }
	.coll-card-btn:hover .edit-hint { opacity: 1; color: var(--accent-blue); }
	.coll-count { margin-left: auto; font-size: 0.8em; color: var(--text-muted); }
	.badge { font-size: 0.7em; background: var(--accent-blue); color: #fff; padding: 1px 6px; border-radius: 10px; font-weight: 600; }
	.field-tags { display: flex; flex-wrap: wrap; gap: var(--space-1); margin-top: var(--space-2); }
	.field-tag { font-size: 0.75em; font-family: var(--font-mono); background: var(--bg-tertiary); color: var(--text-secondary); padding: 1px 8px; border-radius: 10px; }
	.empty-text { color: var(--text-muted); font-size: 0.9em; }
	.theme-row { display: flex; align-items: center; justify-content: space-between; font-size: 0.9em; }
	.theme-toggle { display: flex; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; cursor: pointer; }
	.theme-option { padding: var(--space-1) var(--space-4); font-size: 0.85em; transition: background 0.15s, color 0.15s; }
	.theme-option.active { background: var(--accent-blue); color: #fff; }
	/* ── Members ──── */
	.members-list { display: flex; flex-direction: column; gap: var(--space-3); }
	.member-row { display: flex; align-items: center; justify-content: space-between; padding: var(--space-3) var(--space-4); gap: var(--space-3); }
	.member-info { display: flex; align-items: center; gap: var(--space-3); min-width: 0; }
	.member-avatar { width: 32px; height: 32px; border-radius: 50%; background: var(--accent-blue); color: #fff; display: flex; align-items: center; justify-content: center; font-weight: 600; font-size: 0.85em; flex-shrink: 0; }
	.member-details { display: flex; flex-direction: column; min-width: 0; }
	.member-name { font-weight: 500; font-size: 0.9em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.member-email { font-size: 0.8em; color: var(--text-muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.member-actions { display: flex; align-items: center; gap: var(--space-2); flex-shrink: 0; }
	.role-select { background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius-sm); padding: var(--space-1) var(--space-2); font-size: 0.8em; color: var(--text-primary); cursor: pointer; }
	.role-badge { font-size: 0.8em; background: var(--bg-tertiary); color: var(--text-secondary); padding: 2px 10px; border-radius: 10px; }
	.btn-remove { color: #ef4444; border-color: transparent; background: none; }
	.btn-remove:hover { background: color-mix(in srgb, #ef4444 15%, transparent); }
	.invitations-section { margin-top: var(--space-4); }
	.invitations-section h3 { font-size: 0.9em; color: var(--text-muted); margin-bottom: var(--space-2); }
	.invitation-row { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-2) var(--space-4); font-size: 0.85em; }
	.inv-email { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
	.inv-code { font-family: var(--font-mono); font-size: 0.85em; background: var(--bg-tertiary); padding: 1px 6px; border-radius: var(--radius-sm); color: var(--text-muted); }
	.copy-link-btn { color: var(--accent-blue); border-color: var(--accent-blue); background: none; }
	.copy-link-btn:hover { background: color-mix(in srgb, var(--accent-blue) 15%, transparent); }
	.invite-form { margin-top: var(--space-4); }
	.invite-form h3 { font-size: 0.9em; color: var(--text-secondary); margin-bottom: var(--space-3); }
	.invite-row { display: flex; gap: var(--space-2); align-items: center; flex-wrap: wrap; }
	.invite-row input { flex: 1; min-width: 180px; max-width: 300px; }
	.invite-result { font-size: 0.82em; margin-top: var(--space-2); }
	.invite-success { color: var(--accent-green); }
	.invite-error { color: #ef4444; }
	/* ── Danger Zone ──── */
	.danger-banner { background: color-mix(in srgb, #ef4444 8%, var(--bg-secondary)); border: 1px solid color-mix(in srgb, #ef4444 25%, var(--border)); border-radius: var(--radius); padding: var(--space-3) var(--space-4); margin-bottom: var(--space-4); }
	.danger-banner p { font-size: 0.88em; color: #ef4444; margin: 0; }
	.danger-card { border-color: color-mix(in srgb, #ef4444 30%, var(--border)); }
	.danger-row { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); }
	.danger-info { flex: 1; }
	.danger-info strong { font-size: 0.9em; }
	.danger-info p { font-size: 0.8em; color: var(--text-muted); margin: var(--space-1) 0 0; }
	.btn-danger { padding: var(--space-2) var(--space-4); background: none; border: 1px solid #ef4444; border-radius: var(--radius); color: #ef4444; font-size: 0.85em; cursor: pointer; white-space: nowrap; font-weight: 500; }
	.btn-danger:hover:not(:disabled) { background: #ef4444; color: #fff; }
	.btn-danger:disabled { opacity: 0.4; cursor: not-allowed; }
	.danger-confirm { display: flex; flex-direction: column; gap: var(--space-3); }
	.danger-warning { font-size: 0.88em; color: var(--text-primary); margin: 0; }
	.danger-warning strong { color: #ef4444; }
	.danger-input-row { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
	.slug-hint { font-size: 0.82em; padding: var(--space-1) var(--space-2); background: var(--bg-tertiary); border-radius: var(--radius-sm); color: var(--text-muted); font-family: var(--font-mono); }
	.danger-input { flex: 1; min-width: 180px; max-width: 300px; padding: var(--space-2); font-size: 0.88em; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary); font-family: var(--font-mono); }
	.danger-input:focus { outline: none; border-color: #ef4444; }
	.danger-actions { display: flex; gap: var(--space-2); }
</style>
