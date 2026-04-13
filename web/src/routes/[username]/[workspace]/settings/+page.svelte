<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import type { Collection, WorkspaceContext } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import CreateCollectionModal from '$lib/components/collections/CreateCollectionModal.svelte';
	import EditCollectionModal from '$lib/components/collections/EditCollectionModal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let loading = $state(true);
	let collections = $state<Collection[]>([]);
	let wsName = $state('');
	let savingName = $state(false);
	let nameStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let theme = $state<'light' | 'dark'>('dark');
	let showCreateModal = $state(false);
	let editingCollection = $state<Collection | null>(null);
	let contextEditor = $state('{}');
	let savingContext = $state(false);
	let contextStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let contextError = $state('');

	// Members
	let members = $state<{ user_id: string; user_name: string; user_email: string; role: string }[]>([]);
	let invitations = $state<{ id: string; email: string; role: string; code: string; join_url?: string }[]>([]);
	let inviteEmail = $state('');
	let inviteRole = $state('editor');
	let inviting = $state(false);
	let inviteResult = $state<{ message: string; type: 'success' | 'error' } | null>(null);
	let currentUserRole = $state('');

	// Collection Access
	let expandedAccessUserId = $state<string | null>(null);
	let accessMode = $state<'all' | 'specific'>('all');
	let accessCollectionIds = $state<string[]>([]);
	let accessLoading = $state(false);
	let accessSaving = $state(false);

	// Tabs
	let activeTab = $state('general');
	const tabs = [
		{ id: 'general', label: 'General', icon: '\u2699\uFE0F' },
		{ id: 'members', label: 'Members', icon: '\uD83D\uDC65' },
		{ id: 'collections', label: 'Collections', icon: '\uD83D\uDCC1' },
		{ id: 'danger', label: 'Danger Zone', icon: '\u26A0\uFE0F' },
	];
	let validTabIds = $derived(tabs.map(t => t.id));

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
			contextEditor = JSON.stringify(workspaceStore.current?.context ?? {}, null, 2);
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

	function formatContextEditor(value: WorkspaceContext | null | undefined) {
		return JSON.stringify(value ?? {}, null, 2);
	}

	function resetContextEditor() {
		contextEditor = formatContextEditor(workspaceStore.current?.context);
		contextError = '';
		contextStatus = 'idle';
	}

	function clearContextEditor() {
		contextEditor = '{}';
		contextError = '';
		contextStatus = 'idle';
	}

	function stripContextFromSettings(raw: string | undefined) {
		if (!raw) return '{}';
		try {
			const parsed = JSON.parse(raw) as Record<string, unknown>;
			delete parsed.context;
			return JSON.stringify(parsed);
		} catch {
			return '{}';
		}
	}

	async function saveContext() {
		if (savingContext) return;
		contextError = '';
		contextStatus = 'idle';

		let parsed: unknown;
		try {
			parsed = JSON.parse(contextEditor.trim() || '{}');
		} catch {
			contextError = 'Context must be valid JSON.';
			contextStatus = 'error';
			return;
		}

		if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
			contextError = 'Context must be a JSON object.';
			contextStatus = 'error';
			return;
		}

		savingContext = true;
		try {
			const hasKeys = Object.keys(parsed as Record<string, unknown>).length > 0;
			const updated = hasKeys
				? await api.workspaces.update(wsSlug, { context: parsed as WorkspaceContext })
				: await api.workspaces.update(wsSlug, {
					settings: stripContextFromSettings(workspaceStore.current?.settings)
				});

			await workspaceStore.setCurrent(updated);
			contextEditor = formatContextEditor(updated.context);
			contextStatus = 'saved';
			setTimeout(() => {
				if (contextStatus === 'saved') contextStatus = 'idle';
			}, 2000);
		} catch (err: unknown) {
			contextError = err instanceof Error ? err.message : 'Failed to save workspace context';
			contextStatus = 'error';
		} finally {
			savingContext = false;
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
				copyToClipboard(result.join_url);
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

	async function handleCancelInvitation(invId: string, email: string) {
		if (!confirm(`Cancel invitation for ${email}?`)) return;
		try {
			await api.members.cancelInvitation(wsSlug, invId);
			invitations = invitations.filter(i => i.id !== invId);
			toastStore.show(`Invitation cancelled for ${email}`, 'success');
		} catch {
			toastStore.show('Failed to cancel invitation', 'error');
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

	let nonSystemCollections = $derived(collections.filter(c => !c.is_system));
	let systemCollections = $derived(collections.filter(c => c.is_system));

	async function toggleAccessPanel(userId: string) {
		if (expandedAccessUserId === userId) {
			expandedAccessUserId = null;
			return;
		}
		expandedAccessUserId = userId;
		accessLoading = true;
		accessMode = 'all';
		accessCollectionIds = [];
		try {
			const data = await api.members.getMemberCollectionAccess(wsSlug, userId);
			accessMode = data.collection_access === 'specific' ? 'specific' : 'all';
			accessCollectionIds = data.collection_ids ?? [];
		} catch {
			accessMode = 'all';
			accessCollectionIds = [];
		} finally {
			accessLoading = false;
		}
	}

	function toggleAccessCollection(collId: string) {
		if (accessCollectionIds.includes(collId)) {
			accessCollectionIds = accessCollectionIds.filter(id => id !== collId);
		} else {
			accessCollectionIds = [...accessCollectionIds, collId];
		}
	}

	async function saveCollectionAccess() {
		if (!expandedAccessUserId || accessSaving) return;
		accessSaving = true;
		const userId = expandedAccessUserId;
		const prevMode = accessMode;
		const prevIds = [...accessCollectionIds];
		try {
			const result = await api.members.setMemberCollectionAccess(
				wsSlug,
				userId,
				accessMode,
				accessMode === 'specific' ? accessCollectionIds : []
			);
			accessMode = result.collection_access === 'specific' ? 'specific' : 'all';
			accessCollectionIds = result.collection_ids ?? [];
			toastStore.show('Collection access updated', 'success');
		} catch {
			// Revert on error
			accessMode = prevMode;
			accessCollectionIds = prevIds;
			toastStore.show('Failed to update collection access', 'error');
		} finally {
			accessSaving = false;
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
			goto('/console');
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

	let contextSummary = $derived.by(() => {
		const context = workspaceStore.current?.context;
		if (!context) return [] as { label: string; value: string }[];

		const summary: { label: string; value: string }[] = [];
		if (context.repositories?.length) summary.push({ label: 'Repositories', value: String(context.repositories.length) });
		if (context.commands) {
			const configured = Object.entries(context.commands).filter(([, value]) => Boolean(value)).length;
			if (configured) summary.push({ label: 'Commands', value: String(configured) });
		}
		if (context.stack?.languages?.length) summary.push({ label: 'Languages', value: context.stack.languages.join(', ') });
		if (context.deployment?.mode) summary.push({ label: 'Deployment', value: context.deployment.mode });
		if (context.assumptions?.length) summary.push({ label: 'Assumptions', value: String(context.assumptions.length) });
		return summary;
	});
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
			<section class="section">
				<h2>Workspace Context</h2>
				<p class="section-desc">Machine-readable metadata for repos, commands, stack, deployment, and agent-facing assumptions.</p>
				<div class="card context-card">
					{#if contextSummary.length > 0}
						<div class="context-summary">
							{#each contextSummary as entry (entry.label)}
								<div class="context-chip">
									<span class="context-chip-label">{entry.label}</span>
									<span class="context-chip-value">{entry.value}</span>
								</div>
							{/each}
						</div>
					{/if}
					<label class="context-label" for="workspace-context">Context JSON</label>
					<textarea
						id="workspace-context"
						class="context-editor mono"
						bind:value={contextEditor}
						spellcheck="false"
						rows="18"
					></textarea>
					<p class="context-help">Use a JSON object with keys like <code>repositories</code>, <code>paths</code>, <code>commands</code>, <code>stack</code>, <code>deployment</code>, and <code>assumptions</code>.</p>
					{#if contextError}
						<p class="context-error">{contextError}</p>
					{/if}
					<div class="context-actions">
						<button class="btn btn-primary" onclick={saveContext} disabled={savingContext}>
							{savingContext ? 'Saving...' : 'Save Context'}
						</button>
						<button class="btn" onclick={resetContextEditor} disabled={savingContext}>
							Reset
						</button>
						<button class="btn" onclick={clearContextEditor} disabled={savingContext}>
							Clear
						</button>
						{#if contextStatus === 'saved'}
							<span class="status-saved">Saved</span>
						{:else if contextStatus === 'error' && !contextError}
							<span class="status-error">Error</span>
						{/if}
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
							<div class="member-card-wrapper">
								<div class="card member-row">
									<div class="member-info">
										<span class="member-avatar">{member.user_name.charAt(0).toUpperCase()}</span>
										<div class="member-details">
											<span class="member-name">{member.user_name}</span>
											<span class="member-email">{member.user_email}</span>
										</div>
										{#if expandedAccessUserId === member.user_id && !accessLoading}
											<span class="access-badge" class:access-badge-specific={accessMode === 'specific'}>
												{accessMode === 'all' ? 'All collections' : `${accessCollectionIds.length} collection${accessCollectionIds.length !== 1 ? 's' : ''}`}
											</span>
										{/if}
									</div>
									<div class="member-actions">
										{#if isOwner}
											<button
												class="btn btn-small btn-access"
												class:btn-access-active={expandedAccessUserId === member.user_id}
												onclick={() => toggleAccessPanel(member.user_id)}
											>
												Manage access
											</button>
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
								{#if isOwner && expandedAccessUserId === member.user_id}
									<div class="access-panel">
										{#if accessLoading}
											<p class="access-loading">Loading collection access...</p>
										{:else}
											<div class="access-mode-row">
												<label class="access-mode-label" for="access-mode-{member.user_id}">Collection visibility</label>
												<select
													id="access-mode-{member.user_id}"
													class="role-select"
													value={accessMode}
													onchange={(e) => { accessMode = (e.target as HTMLSelectElement).value as 'all' | 'specific'; }}
												>
													<option value="all">All collections</option>
													<option value="specific">Specific collections</option>
												</select>
											</div>
											{#if accessMode === 'specific'}
												<div class="access-coll-list">
													{#each nonSystemCollections as coll (coll.id)}
														<label class="access-coll-item">
															<input
																type="checkbox"
																checked={accessCollectionIds.includes(coll.id)}
																onchange={() => toggleAccessCollection(coll.id)}
															/>
															<span class="access-coll-icon">{coll.icon || '#'}</span>
															<span class="access-coll-name">{coll.name}</span>
														</label>
													{/each}
													{#each systemCollections as coll (coll.id)}
														<label class="access-coll-item access-coll-system" title="System collection — always visible">
															<input type="checkbox" checked disabled />
															<span class="access-coll-icon">{coll.icon || '#'}</span>
															<span class="access-coll-name">{coll.name}</span>
															<span class="access-system-tag">system</span>
														</label>
													{/each}
												</div>
											{/if}
											<div class="access-actions">
												<button class="btn btn-primary btn-small" onclick={saveCollectionAccess} disabled={accessSaving}>
													{accessSaving ? 'Saving...' : 'Save'}
												</button>
												<button class="btn btn-small" onclick={() => { expandedAccessUserId = null; }}>
													Cancel
												</button>
											</div>
										{/if}
									</div>
								{/if}
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
								{#if inv.join_url || inv.code}
									<button class="btn btn-small copy-link-btn" onclick={async () => { const url = inv.join_url || `${window.location.origin}/join/${inv.code}`; const ok = await copyToClipboard(url); toastStore.show(ok ? 'Link copied!' : 'Failed to copy link', ok ? 'success' : 'error'); }}>
										Copy invite link
									</button>
								{:else}
									<span class="inv-sent-label">Sent via email</span>
								{/if}
								{#if isOwner}
									<button class="btn btn-small btn-remove" onclick={() => handleCancelInvitation(inv.id, inv.email)}>
										Cancel
									</button>
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
	.context-card { display: flex; flex-direction: column; gap: var(--space-3); }
	.context-summary { display: flex; flex-wrap: wrap; gap: var(--space-2); }
	.context-chip { display: inline-flex; align-items: center; gap: var(--space-2); background: var(--bg-tertiary); border: 1px solid var(--border-subtle); border-radius: 999px; padding: var(--space-1) var(--space-3); font-size: 0.78em; }
	.context-chip-label { color: var(--text-muted); }
	.context-chip-value { color: var(--text-primary); font-weight: 600; }
	.context-label { font-size: 0.82em; color: var(--text-secondary); }
	.context-editor { width: 100%; min-height: 320px; resize: vertical; padding: var(--space-3); background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary); line-height: 1.5; }
	.context-editor:focus { outline: none; border-color: var(--accent-blue); }
	.context-help { margin: 0; font-size: 0.8em; color: var(--text-muted); }
	.context-help code { font-family: var(--font-mono); font-size: 0.95em; }
	.context-error { margin: 0; font-size: 0.82em; color: #ef4444; }
	.context-actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
	/* ── Members ──── */
	.members-list { display: flex; flex-direction: column; }
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
	/* ── Collection Access ──── */
	.member-card-wrapper { display: flex; flex-direction: column; }
	.member-card-wrapper + .member-card-wrapper { margin-top: var(--space-3); }
	.member-card-wrapper .card.member-row { border-radius: var(--radius); }
	.member-card-wrapper:has(.access-panel) .card.member-row { border-bottom-left-radius: 0; border-bottom-right-radius: 0; border-bottom-color: transparent; }
	.access-badge { font-size: 0.72em; padding: 2px 8px; border-radius: 10px; background: var(--bg-tertiary); color: var(--text-muted); white-space: nowrap; margin-left: var(--space-2); }
	.access-badge-specific { background: color-mix(in srgb, var(--accent-blue) 15%, transparent); color: var(--accent-blue); }
	.btn-access { color: var(--accent-blue); border-color: var(--accent-blue); background: none; }
	.btn-access:hover { background: color-mix(in srgb, var(--accent-blue) 10%, transparent); }
	.btn-access-active { background: color-mix(in srgb, var(--accent-blue) 15%, transparent); }
	.access-panel { background: color-mix(in srgb, var(--bg-secondary) 80%, var(--bg-tertiary)); border: 1px solid var(--border); border-top: none; border-bottom-left-radius: var(--radius); border-bottom-right-radius: var(--radius); padding: var(--space-4); display: flex; flex-direction: column; gap: var(--space-3); }
	.access-loading { font-size: 0.85em; color: var(--text-muted); margin: 0; }
	.access-mode-row { display: flex; align-items: center; gap: var(--space-3); }
	.access-mode-label { font-size: 0.85em; color: var(--text-secondary); white-space: nowrap; }
	.access-coll-list { display: flex; flex-direction: column; gap: var(--space-1); max-height: 240px; overflow-y: auto; padding: var(--space-2); background: var(--bg-tertiary); border: 1px solid var(--border-subtle); border-radius: var(--radius); }
	.access-coll-item { display: flex; align-items: center; gap: var(--space-2); padding: var(--space-1) var(--space-2); border-radius: var(--radius-sm); cursor: pointer; font-size: 0.85em; }
	.access-coll-item:hover { background: var(--bg-hover); }
	.access-coll-item input[type="checkbox"] { margin: 0; cursor: pointer; }
	.access-coll-icon { font-size: 0.95em; }
	.access-coll-name { flex: 1; }
	.access-coll-system { opacity: 0.6; cursor: default; }
	.access-coll-system:hover { background: none; }
	.access-system-tag { font-size: 0.7em; background: var(--bg-secondary); color: var(--text-muted); padding: 1px 5px; border-radius: 8px; }
	.access-actions { display: flex; align-items: center; gap: var(--space-2); }
	.invitations-section { margin-top: var(--space-4); }
	.invitations-section h3 { font-size: 0.9em; color: var(--text-muted); margin-bottom: var(--space-2); }
	.invitation-row { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-2) var(--space-4); font-size: 0.85em; }
	.inv-email { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
	.copy-link-btn { color: var(--accent-blue); border-color: var(--accent-blue); background: none; }
	.copy-link-btn:hover { background: color-mix(in srgb, var(--accent-blue) 15%, transparent); }
	.inv-sent-label { font-size: 0.8em; color: var(--text-muted); font-style: italic; }
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
	.section-desc { font-size: 0.85em; color: var(--text-muted); margin-bottom: var(--space-3); }
</style>
