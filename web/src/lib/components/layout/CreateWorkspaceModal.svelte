<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Workspace, WorkspaceTemplate } from '$lib/types';
	import { groupTemplatesByCategory } from '$lib/utils/templates';

	interface Props {
		/**
		 * Optional callback fired after a successful create OR import, before
		 * the modal closes and the user is navigated to the new workspace.
		 *
		 * Phase 1 (TASK-1528) defines the API; Phase F (TASK-1526) wires it
		 * to `uiStore.requestConnectAfterNavigate(ws.slug)` so the workspace's
		 * +page.svelte auto-opens ConnectWorkspaceModal once the user lands
		 * on the new workspace. The modal still owns navigation + close —
		 * the callback is side-effect-only.
		 */
		onWorkspaceCreated?: (workspace: Workspace) => void;
	}

	let { onWorkspaceCreated }: Props = $props();

	// Default selection is `blank` per IDEA-1516 §2 — agent-driven onboarding
	// is the recommended path, the primary "Start blank" card represents that.
	// Picking any template card replaces 'blank' with that template's name.
	let mode = $state<'create' | 'import'>('create');
	let newName = $state('');
	// Optional "what are you tracking?" intent captured at creation. Stored as
	// the workspace description and surfaced in the bootstrap blob so the
	// onboard playbook starts the interview warm (PLAN-1847 Phase 3 / TASK-1855).
	let newDescription = $state('');
	let selectedTemplate = $state('blank');
	// Independent of `selectedTemplate` — collapsed by default per IDEA-1516
	// §2. Users who expand and pick a template flip `selectedTemplate` to
	// that template; users who never expand keep `blank`.
	let templatesExpanded = $state(false);
	let templates = $state<WorkspaceTemplate[]>([]);
	let loadingTemplates = $state(false);
	let importing = $state(false);
	let importFile = $state<File | null>(null);
	let fileInputEl = $state<HTMLInputElement>();
	let nameInputEl = $state<HTMLInputElement>();
	let dragging = $state(false);
	let dragCounter = 0;

	// Group by category and filter out Blank — the primary card above replaces
	// it. If the templates list ever ships without a `blank` entry we'd still
	// behave correctly (filter is a no-op).
	let grouped = $derived(
		groupTemplatesByCategory(templates).map((g) => ({
			...g,
			templates: g.templates.filter((t) => t.name !== 'blank'),
		})).filter((g) => g.templates.length > 0)
	);

	$effect(() => {
		if (uiStore.createWorkspaceOpen) {
			// Reset state on open
			mode = 'create';
			newName = '';
			newDescription = '';
			selectedTemplate = 'blank';
			templatesExpanded = false;
			importFile = null;
			importing = false;
			// Load templates
			if (templates.length === 0) {
				loadingTemplates = true;
				api.templates.list().then(t => templates = t).catch(() => {}).finally(() => loadingTemplates = false);
			}
			// Focus name input
			requestAnimationFrame(() => nameInputEl?.focus());
		}
	});

	function close() {
		uiStore.closeCreateWorkspace();
	}

	function selectBlank() {
		selectedTemplate = 'blank';
	}

	function pickTemplate(name: string) {
		selectedTemplate = name;
	}

	function toggleTemplates() {
		templatesExpanded = !templatesExpanded;
		// First time the user expands the section, pre-select `startup` to
		// match the pre-IDEA-1516 default behavior — they obviously want a
		// template if they bothered expanding, and `blank` would still be
		// selected from the primary card otherwise. We only do this when
		// the current selection is still `blank` (user hasn't picked yet)
		// AND startup is actually in the templates list.
		if (templatesExpanded && selectedTemplate === 'blank') {
			if (templates.some((t) => t.name === 'startup')) {
				selectedTemplate = 'startup';
			}
		}
	}

	async function createWorkspace() {
		if (!newName.trim()) return;
		try {
			const ws = await workspaceStore.create({
				name: newName.trim(),
				description: newDescription.trim() || undefined,
				template: selectedTemplate || undefined
			});
			// Fire the Phase F hook BEFORE close + goto so the consumer can
			// stage state (e.g. uiStore.requestConnectAfterNavigate) that
			// the destination route will read on mount. Callback is purely
			// for side effects — navigation stays our responsibility.
			onWorkspaceCreated?.(ws);
			close();
			goto(`/${ws.owner_username}/${ws.slug}`);
		} catch (err: unknown) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				toastStore.show('Failed to create workspace', 'error');
			}
		}
	}

	async function importWorkspace() {
		if (!importFile) return;
		importing = true;
		try {
			const ws = await api.workspaces.importBundle(importFile, newName.trim() || undefined);
			await workspaceStore.loadAll();
			// Same Phase F hook as create — claim code is equally useful for
			// imported workspaces, and the user explicitly opted into this
			// modal so opening the Connect modal post-import isn't surprising.
			onWorkspaceCreated?.(ws);
			close();
			toastStore.show(`Imported workspace "${ws.name}"`, 'success');
			goto(`/${ws.owner_username}/${ws.slug}`);
		} catch (err) {
			toastStore.show(`Import failed: ${err instanceof Error ? err.message : 'Unknown error'}`, 'error');
		} finally {
			importing = false;
		}
	}

	function isAcceptedBundleFile(name: string): boolean {
		// Web UI is strict tar.gz-only. The new-workspace flow always
		// POSTs as Content-Type: application/gzip so a dropped .json
		// would route to the bundle path and fail with a gzip decode
		// error — confusing UX. Operators with a legacy JSON export
		// can still curl it against POST /workspaces/import directly;
		// the server keeps the JSON dispatch for back-compat.
		return /(\.tar\.gz|\.tgz)$/i.test(name);
	}

	function setFile(file: File) {
		importFile = file;
		mode = 'import';
		if (!newName.trim()) {
			newName = file.name.replace(/(-export\.tar\.gz$|\.tar\.gz$|\.tgz$)/i, '');
		}
	}

	function handleFileSelect(e: Event) {
		const input = e.target as HTMLInputElement;
		const file = input.files?.[0];
		if (file) setFile(file);
	}

	function handleDrop(e: DragEvent) {
		e.preventDefault();
		dragCounter = 0;
		dragging = false;
		const file = e.dataTransfer?.files[0];
		if (file && isAcceptedBundleFile(file.name)) setFile(file);
	}

	function handleDragOver(e: DragEvent) {
		e.preventDefault();
	}

	function handleDragEnter(e: DragEvent) {
		e.preventDefault();
		dragCounter++;
		dragging = true;
	}

	function handleDragLeave() {
		dragCounter--;
		if (dragCounter <= 0) {
			dragCounter = 0;
			dragging = false;
		}
	}
</script>

{#if uiStore.createWorkspaceOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="modal-backdrop" onclick={close}></div>
	<div class="modal" role="dialog">
		<div class="modal-header">
			<h2>New Workspace</h2>
			<button class="modal-close" onclick={close}>✕</button>
		</div>

		<div class="modal-tabs">
			<button class="tab" class:active={mode === 'create'} onclick={() => mode = 'create'}>Create</button>
			<button class="tab" class:active={mode === 'import'} onclick={() => mode = 'import'}>Import</button>
		</div>

		<div class="modal-body">
			<label class="field-label" for="ws-create-name">Name</label>
			<input
				id="ws-create-name"
				bind:this={nameInputEl}
				bind:value={newName}
				placeholder="Workspace name"
				onkeydown={(e) => e.key === 'Enter' && (mode === 'create' ? createWorkspace() : importWorkspace())}
			/>

			{#if mode === 'create'}
				<label class="field-label" for="ws-create-desc">What are you tracking? <span class="field-optional">(optional)</span></label>
				<textarea
					id="ws-create-desc"
					class="desc-input"
					bind:value={newDescription}
					rows="2"
					placeholder="e.g. a SvelteKit SaaS app, my job search, a research project on…"
				></textarea>
				<p class="field-hint">Your agent uses this to set the workspace up for you.</p>

				<!--
					Primary "Start blank" card — visually selected when
					selectedTemplate === 'blank'. Clicking it returns to
					the blank selection from any template pick. Per IDEA-1516
					§2: agent-driven onboarding is the recommended path.
				-->
				<button
					type="button"
					class="blank-card"
					class:selected={selectedTemplate === 'blank'}
					onclick={selectBlank}
					aria-pressed={selectedTemplate === 'blank'}
				>
					<span class="blank-card-icon" aria-hidden="true">✨</span>
					<span class="blank-card-text">
						<span class="blank-card-title">Start blank</span>
						<span class="blank-card-desc">
							Recommended. Just conventions and playbooks to start. Your agent
							will walk you through setting up the rest.
						</span>
					</span>
				</button>

				<!--
					Templates section — collapsed by default. Header is a
					button so keyboard users can toggle it; aria-expanded
					mirrors visual state for AT.
				-->
				<button
					type="button"
					class="templates-toggle"
					onclick={toggleTemplates}
					aria-expanded={templatesExpanded}
					aria-controls="ws-create-templates"
				>
					<span class="templates-toggle-label">Or pick a template</span>
					<span class="templates-toggle-chevron" class:expanded={templatesExpanded} aria-hidden="true">▾</span>
				</button>

				{#if templatesExpanded}
					<div id="ws-create-templates" class="templates-section">
						{#if loadingTemplates && templates.length === 0}
							<span class="templates-loading">Loading templates…</span>
						{:else if grouped.length === 0}
							<span class="templates-loading">No templates available.</span>
						{:else}
							<div class="template-list">
								{#each grouped as group (group.category)}
									<span class="cat-label">{group.label}</span>
									{#each group.templates as tpl (tpl.name)}
										<button
											type="button"
											class="template-card"
											class:selected={selectedTemplate === tpl.name}
											onclick={() => pickTemplate(tpl.name)}
										>
											{#if tpl.icon}
												<span class="tpl-icon">{tpl.icon}</span>
											{/if}
											<span class="tpl-text">
												<span class="tpl-name">{tpl.name}</span>
												<span class="tpl-desc">{tpl.collections.join(', ')}</span>
											</span>
										</button>
									{/each}
								{/each}
							</div>
							<!--
								Footnote per IDEA-1516 §2 — reminds users that
								the agent flow still applies even if they
								picked a template. Mirrors the locked-decisions
								copy verbatim.
							-->
							<p class="templates-footnote">
								Picked a template? Your agent can still adapt it later — type
								<code>/pad onboard</code>.
							</p>
						{/if}
					</div>
				{/if}
			{:else}
				<span class="field-label">Export file</span>
				<div
					role="button"
					tabindex="0"
					class="drop-zone"
					class:dragging
					class:has-file={!!importFile}
					onclick={() => fileInputEl?.click()}
					onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInputEl?.click(); } }}
					ondrop={handleDrop}
					ondragover={handleDragOver}
					ondragenter={handleDragEnter}
					ondragleave={handleDragLeave}
				>
					{#if importFile}
						<span class="drop-file-name">{importFile.name}</span>
						<span class="drop-hint">Click or drop to replace</span>
					{:else}
						<span class="drop-icon" class:drop-icon-active={dragging}>↓</span>
						<span class="drop-text">{dragging ? 'Drop bundle (.tar.gz) here' : 'Drop workspace bundle (.tar.gz) here or click to browse'}</span>
					{/if}
					<input
						bind:this={fileInputEl}
						type="file"
						accept=".tar.gz,.tgz,application/gzip,application/x-gzip"
						oninput={handleFileSelect}
						style="display:none"
					/>
				</div>
				{#if importFile}
					<p class="import-hint">Creates a new workspace with regenerated IDs (items, comments, attachments, version history all preserved). Original data is unchanged.</p>
				{/if}
			{/if}
		</div>

		<div class="modal-footer">
			<button class="cancel-btn" onclick={close}>Cancel</button>
			{#if mode === 'create'}
				<button class="action-btn" onclick={createWorkspace} disabled={!newName.trim()}>
					Create Workspace
				</button>
			{:else}
				<button class="action-btn" onclick={importWorkspace} disabled={!importFile || importing}>
					{importing ? 'Importing...' : 'Import Workspace'}
				</button>
			{/if}
		</div>
	</div>
{/if}

<style>
	.modal-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 200;
	}
	.modal {
		position: fixed;
		top: 50%;
		left: 50%;
		transform: translate(-50%, -50%);
		width: 90%;
		max-width: 480px;
		max-height: 85vh;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 16px 48px rgba(0, 0, 0, 0.4);
		z-index: 201;
		display: flex;
		flex-direction: column;
		overflow: hidden;
		animation: modal-in 0.15s ease-out;
	}
	@keyframes modal-in {
		from { opacity: 0; transform: translate(-50%, -48%); }
		to { opacity: 1; transform: translate(-50%, -50%); }
	}
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}
	.modal-header h2 { margin: 0; font-size: 1.1em; }
	.modal-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1.1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
	}
	.modal-close:hover { background: var(--bg-hover); color: var(--text-primary); }

	.modal-tabs {
		display: flex;
		border-bottom: 1px solid var(--border);
	}
	.tab {
		flex: 1;
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		font-size: 0.88em;
		font-weight: 500;
		color: var(--text-muted);
		cursor: pointer;
		border-bottom: 2px solid transparent;
		transition: color 0.15s, border-color 0.15s;
	}
	.tab:hover { color: var(--text-secondary); }
	.tab.active { color: var(--accent-blue); border-bottom-color: var(--accent-blue); }

	.modal-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		overflow-y: auto;
	}
	.field-label {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
	.modal-body input:not([type]) {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
	}
	.modal-body input:focus { outline: none; border-color: var(--accent-blue); }

	.field-optional {
		text-transform: none;
		font-weight: 400;
		letter-spacing: 0;
		color: var(--text-muted);
	}
	.desc-input {
		width: 100%;
		box-sizing: border-box;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9em;
		font-family: inherit;
		resize: vertical;
	}
	.desc-input:focus { outline: none; border-color: var(--accent-blue); }
	.field-hint {
		margin: calc(-1 * var(--space-1)) 0 0;
		font-size: 0.78em;
		color: var(--text-muted);
	}

	/* Primary "Start blank" card — bigger than a template card to anchor it
	   as the recommended option. Same selection treatment (accent border +
	   tinted background) for visual consistency with the template grid. */
	.blank-card {
		display: flex;
		flex-direction: row;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		text-align: left;
		cursor: pointer;
		border: 1px solid transparent;
		transition: border-color 0.1s, background 0.1s;
		width: 100%;
		margin-top: var(--space-1);
	}
	.blank-card:hover { border-color: var(--border); }
	.blank-card.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.blank-card-icon {
		font-size: 1.4em;
		line-height: 1.2;
		flex-shrink: 0;
	}
	.blank-card-text {
		display: flex;
		flex-direction: column;
		gap: 4px;
		min-width: 0;
	}
	.blank-card-title {
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.blank-card-desc {
		font-size: 0.82em;
		color: var(--text-muted);
		line-height: 1.45;
	}

	/* Toggle row — flat button with chevron that rotates when expanded. */
	.templates-toggle {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: none;
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		font-size: 0.88em;
		font-weight: 500;
		cursor: pointer;
		text-align: left;
		width: 100%;
		transition: color 0.1s, background 0.1s;
	}
	.templates-toggle:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.templates-toggle-label { flex: 1; }
	.templates-toggle-chevron {
		font-size: 0.9em;
		color: var(--text-muted);
		transition: transform 0.15s;
	}
	.templates-toggle-chevron.expanded { transform: rotate(180deg); }

	.templates-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.templates-loading {
		font-size: 0.85em;
		color: var(--text-muted);
		padding: var(--space-2) var(--space-3);
	}
	.templates-footnote {
		margin: var(--space-2) 0 0;
		font-size: 0.8em;
		color: var(--text-muted);
		line-height: 1.5;
	}
	.templates-footnote code {
		background: var(--bg-tertiary);
		padding: 1px 4px;
		border-radius: var(--radius-sm);
		font-size: 0.95em;
	}

	.template-list { display: flex; flex-direction: column; gap: 4px; }
	.cat-label {
		font-size: 0.72em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		opacity: 0.8;
		margin-top: var(--space-2);
	}
	.cat-label:first-child { margin-top: 0; }
	.template-card {
		display: flex; flex-direction: row; align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		text-align: left; cursor: pointer;
		border: 1px solid transparent;
		transition: border-color 0.1s;
	}
	.template-card:hover { border-color: var(--border); }
	.template-card.selected {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.tpl-icon {
		font-size: 1.1em;
		margin-right: var(--space-2);
		flex-shrink: 0;
	}
	.tpl-text { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
	.tpl-name { font-size: 0.88em; font-weight: 600; color: var(--text-primary); text-transform: capitalize; }
	.tpl-desc { font-size: 0.78em; color: var(--text-muted); }

	.drop-zone {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		min-height: 90px;
		padding: var(--space-4);
		background: var(--bg-tertiary);
		border: 2px dashed var(--border);
		border-radius: var(--radius);
		cursor: pointer;
		transition: border-color 0.15s, background 0.15s;
		text-align: center;
	}
	.drop-zone:hover { border-color: var(--accent-blue); }
	.drop-zone.dragging {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 8%, var(--bg-tertiary));
	}
	.drop-zone.has-file { border-style: solid; border-color: var(--accent-green); }
	.drop-icon { font-size: 1.4em; color: var(--text-muted); transition: color 0.15s, transform 0.15s; }
	.drop-icon-active { color: var(--accent-blue); transform: translateY(2px); }
	.drop-text { font-size: 0.82em; color: var(--text-muted); }
	.drop-file-name { font-size: 0.88em; font-weight: 500; color: var(--text-primary); }
	.drop-hint { font-size: 0.75em; color: var(--text-muted); }
	.import-hint { font-size: 0.8em; color: var(--text-muted); margin: 0; }

	.modal-footer {
		display: flex; align-items: center; justify-content: flex-end;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-top: 1px solid var(--border);
	}
	.action-btn {
		background: var(--accent-blue); color: #fff;
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.88em; font-weight: 500; cursor: pointer;
	}
	.action-btn:hover:not(:disabled) { filter: brightness(1.1); }
	.action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
	.cancel-btn {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		font-size: 0.88em; color: var(--text-muted);
		background: var(--bg-tertiary);
		border: 1px solid var(--border); cursor: pointer;
	}
	.cancel-btn:hover { background: var(--bg-hover); color: var(--text-primary); }

	/* Mobile: tighter padding inside the modal so the blank card body and the
	   expanded template categories don't overflow horizontally at 480px. */
	@media (max-width: 480px) {
		.modal-body { padding: var(--space-4); }
		.blank-card { padding: var(--space-3); }
		.blank-card-desc { font-size: 0.8em; }
	}
</style>
