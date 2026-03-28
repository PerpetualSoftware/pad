<script lang="ts">
	import type { Version, Item, Activity } from '$lib/types';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import DiffView from './DiffView.svelte';
	import { relativeTime } from '$lib/utils/markdown';

	interface Props {
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
		onRestore?: (item: Item) => void;
		onClose?: () => void;
	}

	let { wsSlug, itemSlug, currentContent, onRestore, onClose }: Props = $props();

	/** A unified timeline entry -- either a content version or an activity event. */
	interface TimelineEntry {
		id: string;
		kind: 'version' | 'activity';
		created_at: string;
		actor: string;
		actor_name: string;
		source: string;
		summary: string;
		/** Only for version entries */
		version?: Version;
		/** Only for activity entries */
		activity?: Activity;
	}

	let versions = $state<Version[]>([]);
	let activities = $state<Activity[]>([]);
	let loading = $state(true);
	let error = $state('');
	let selectedEntryId = $state<string | null>(null);
	let confirmingRestoreId = $state<string | null>(null);
	let restoringId = $state<string | null>(null);

	/** Merge versions and activities into a single timeline sorted newest-first. */
	let timeline = $derived.by(() => {
		const entries: TimelineEntry[] = [];

		// Track version timestamps to avoid showing duplicate activity entries
		const versionTimestamps = new Set<number>();
		for (const v of versions) {
			versionTimestamps.add(Math.floor(new Date(v.created_at).getTime() / 1000));
		}

		// Add version entries
		for (const v of versions) {
			entries.push({
				id: v.id,
				kind: 'version',
				created_at: v.created_at,
				actor: v.created_by,
				actor_name: '',
				source: v.source,
				summary: v.change_summary || 'Content updated',
				version: v
			});
		}

		// Filter activity entries:
		// 1. Skip "updated" activities that have a matching version at the same second
		// 2. Collapse rapid content autosaves (empty-metadata "updated" entries within 5 min)
		//    into a single entry to reduce noise
		const sortedActivities = [...activities].sort(
			(a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
		);

		let lastContentSaveTime = 0;
		for (const a of sortedActivities) {
			const aTime = Math.floor(new Date(a.created_at).getTime() / 1000);
			const hasMatchingVersion = a.action === 'updated' && versionTimestamps.has(aTime);
			if (hasMatchingVersion) continue;

			// Collapse rapid content autosaves: if this is an "updated" with no meaningful
			// metadata and is within 5 minutes of the last one we kept, skip it
			const isEmptyUpdate = a.action === 'updated' && (!a.metadata || a.metadata === '{}');
			if (isEmptyUpdate) {
				if (lastContentSaveTime > 0 && (lastContentSaveTime - aTime) < 300) {
					continue; // Skip -- too close to a newer save we already kept
				}
				lastContentSaveTime = aTime;
			}

			entries.push({
				id: `activity-${a.id}`,
				kind: 'activity',
				created_at: a.created_at,
				actor: a.actor,
				actor_name: a.actor_name ?? '',
				source: a.source,
				summary: formatActivitySummary(a),
				activity: a
			});
		}

		// Sort newest first
		entries.sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime());
		return entries;
	});

	let selectedEntry = $derived(
		selectedEntryId ? timeline.find((e) => e.id === selectedEntryId) ?? null : null
	);

	let diffOldContent = $derived.by(() => {
		if (!selectedEntry?.version) return '';
		return selectedEntry.version.content;
	});

	let diffNewContent = $derived.by(() => {
		if (!selectedEntry?.version) return '';
		const idx = versions.findIndex((v) => v.id === selectedEntry!.version!.id);
		if (idx <= 0) {
			return currentContent;
		}
		return versions[idx - 1].content;
	});

	$effect(() => {
		loadHistory();
	});

	async function loadHistory() {
		loading = true;
		error = '';
		try {
			const [versionResult, activityResult] = await Promise.all([
				api.versions.list(wsSlug, itemSlug),
				api.versions.activity(wsSlug, itemSlug)
			]);
			versions = versionResult.sort(
				(a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
			);
			activities = activityResult;
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load history';
		} finally {
			loading = false;
		}
	}

	function formatActivitySummary(a: Activity): string {
		const actionLabels: Record<string, string> = {
			created: 'Item created',
			updated: 'Item updated',
			archived: 'Item archived',
			restored: 'Item restored',
			moved: 'Item moved'
		};
		let label = actionLabels[a.action] ?? a.action;

		// Parse metadata for richer descriptions
		if (a.metadata && a.metadata !== '{}') {
			try {
				const meta = JSON.parse(a.metadata);
				if (meta.changes) {
					label = meta.changes;
				}
				if (meta.from_collection && meta.to_collection) {
					label = `Moved from ${meta.from_collection} to ${meta.to_collection}`;
				}
			} catch { /* ignore parse errors */ }
		}
		return label;
	}

	function selectEntry(id: string) {
		if (selectedEntryId === id) {
			selectedEntryId = null;
		} else {
			selectedEntryId = id;
			confirmingRestoreId = null;
		}
	}

	function startRestore(id: string) {
		confirmingRestoreId = id;
	}

	function cancelRestore() {
		confirmingRestoreId = null;
	}

	async function confirmRestore(versionId: string) {
		restoringId = versionId;
		try {
			const updatedItem = await api.versions.restore(wsSlug, itemSlug, versionId);
			toastStore.show('Version restored successfully', 'success');
			confirmingRestoreId = null;
			onRestore?.(updatedItem);
		} catch (err) {
			const message = err instanceof Error ? err.message : 'Failed to restore version';
			toastStore.show(message, 'error');
		} finally {
			restoringId = null;
		}
	}

	function actorLabel(entry: TimelineEntry): string {
		if (entry.actor_name) return entry.actor_name;
		return entry.actor === 'agent' ? 'Agent' : 'User';
	}

	function sourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill'
		};
		return labels[source] ?? source;
	}
</script>

<div class="version-panel">
	<div class="panel-header">
		<h3>History</h3>
		{#if onClose}
			<button class="close-btn" type="button" onclick={onClose}>&#10005;</button>
		{/if}
	</div>

	<div class="panel-body">
		{#if loading}
			<div class="loading">
				<span class="spinner"></span>
				<span>Loading history...</span>
			</div>
		{:else if error}
			<div class="error-msg">{error}</div>
		{:else if timeline.length === 0}
			<div class="empty-state">
				<span class="empty-icon">&#128196;</span>
				<p>No history yet</p>
				<p class="empty-hint">Changes to this item will appear here automatically.</p>
			</div>
		{:else}
			<div class="timeline">
				{#each timeline as entry, i (entry.id)}
					{@const isSelected = selectedEntryId === entry.id}
					{@const isVersion = entry.kind === 'version'}
					{@const isConfirming = confirmingRestoreId === entry.id}
					{@const isRestoring = restoringId === entry.id}

					<div class="timeline-entry" class:selected={isSelected}>
						<div class="timeline-marker">
							<div
								class="marker-dot"
								class:active={isSelected}
								class:marker-version={isVersion}
							></div>
							{#if i < timeline.length - 1}
								<div class="marker-line"></div>
							{/if}
						</div>

						<div class="entry-content">
							{#if isVersion}
								<button
									class="entry-header"
									type="button"
									onclick={() => selectEntry(entry.id)}
								>
									<div class="entry-meta">
										<span class="entry-time" title={new Date(entry.created_at).toLocaleString()}>{relativeTime(entry.created_at)}</span>
										<div class="badges">
											<span class="badge badge-version">Content</span>
											<span
												class="badge"
												class:badge-agent={entry.actor === 'agent'}
												class:badge-user={entry.actor !== 'agent'}
											>
												{actorLabel(entry)}
											</span>
											<span class="badge badge-source">
												{sourceLabel(entry.source)}
											</span>
										</div>
									</div>
									{#if entry.summary}
										<p class="change-summary">{entry.summary}</p>
									{/if}
								</button>

								{#if isSelected}
									<div class="entry-details">
										<div class="diff-container">
											<DiffView oldContent={diffOldContent} newContent={diffNewContent} />
										</div>

										<div class="restore-area">
											{#if isConfirming}
												<div class="confirm-prompt">
													<span class="confirm-text">Restore to this version?</span>
													<div class="confirm-actions">
														<button
															class="btn-cancel"
															type="button"
															onclick={cancelRestore}
															disabled={isRestoring}
														>
															Cancel
														</button>
														<button
															class="btn-restore-confirm"
															type="button"
															onclick={() => confirmRestore(entry.version!.id)}
															disabled={isRestoring}
														>
															{isRestoring ? 'Restoring...' : 'Confirm Restore'}
														</button>
													</div>
												</div>
											{:else}
												<button
													class="btn-restore"
													type="button"
													onclick={() => startRestore(entry.id)}
												>
													Restore this version
												</button>
											{/if}
										</div>
									</div>
								{/if}
							{:else}
								<!-- Activity entry (non-expandable) -->
								<div class="entry-header activity-entry">
									<div class="entry-meta">
										<span class="entry-time" title={new Date(entry.created_at).toLocaleString()}>{relativeTime(entry.created_at)}</span>
										<div class="badges">
											<span
												class="badge"
												class:badge-agent={entry.actor === 'agent'}
												class:badge-user={entry.actor !== 'agent'}
											>
												{actorLabel(entry)}
											</span>
											<span class="badge badge-source">
												{sourceLabel(entry.source)}
											</span>
										</div>
									</div>
									<p class="change-summary">{entry.summary}</p>
								</div>
							{/if}
						</div>
					</div>
				{/each}
			</div>
		{/if}
	</div>
</div>

<style>
	.version-panel {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
		display: flex;
		flex-direction: column;
		max-height: 100%;
	}

	.panel-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.panel-header h3 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
	}

	.close-btn:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}

	.panel-body {
		overflow-y: auto;
		flex: 1;
		padding: var(--space-3) var(--space-4);
	}

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) 0;
		color: var(--text-muted);
		font-size: 0.9em;
		justify-content: center;
	}

	.spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to { transform: rotate(360deg); }
	}

	.error-msg {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	.empty-state {
		text-align: center;
		padding: var(--space-6) var(--space-4);
		color: var(--text-muted);
	}

	.empty-icon {
		font-size: 2em;
		display: block;
		margin-bottom: var(--space-2);
	}

	.empty-state p { margin: 0; font-size: 0.9em; }

	.empty-hint {
		margin-top: var(--space-1) !important;
		font-size: 0.8em !important;
		color: var(--text-muted);
	}

	.timeline { display: flex; flex-direction: column; }

	.timeline-entry {
		display: flex;
		gap: var(--space-3);
		position: relative;
	}

	.timeline-marker {
		display: flex;
		flex-direction: column;
		align-items: center;
		flex-shrink: 0;
		width: 16px;
		padding-top: var(--space-2);
	}

	.marker-dot {
		width: 10px;
		height: 10px;
		border-radius: 50%;
		background: var(--bg-tertiary);
		border: 2px solid var(--border);
		flex-shrink: 0;
		z-index: 1;
	}

	.marker-dot.active {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
	}

	.marker-dot.marker-version {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		width: 12px;
		height: 12px;
	}

	.marker-line {
		width: 2px;
		flex: 1;
		background: var(--border);
		min-height: var(--space-2);
	}

	.entry-content {
		flex: 1;
		min-width: 0;
		padding-bottom: var(--space-3);
	}

	.entry-header {
		width: 100%;
		background: none;
		border: 1px solid transparent;
		border-radius: var(--radius);
		padding: var(--space-2);
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
	}

	.entry-header:hover { background: var(--bg-tertiary); }

	.entry-header.activity-entry { cursor: default; }
	.entry-header.activity-entry:hover { background: none; }

	.selected .entry-header {
		background: var(--bg-tertiary);
		border-color: var(--border);
	}

	.entry-meta {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}

	.entry-time {
		font-size: 0.85em;
		color: var(--text-secondary);
		font-weight: 500;
	}

	.badges { display: flex; gap: var(--space-1); }

	.badge {
		display: inline-flex;
		align-items: center;
		padding: 1px var(--space-2);
		border-radius: 9999px;
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		line-height: 1.6;
	}

	.badge-agent {
		background: color-mix(in srgb, var(--accent-purple, #a855f7) 15%, transparent);
		color: var(--accent-purple, #a855f7);
	}

	.badge-user {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.badge-source {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.badge-version {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.change-summary {
		margin: var(--space-1) 0 0 0;
		font-size: 0.8em;
		color: var(--text-muted);
		line-height: 1.4;
	}

	.entry-details {
		margin-top: var(--space-2);
		padding: 0 var(--space-2);
	}

	.diff-container {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
		margin-bottom: var(--space-3);
	}

	.restore-area { display: flex; justify-content: flex-end; }

	.btn-restore {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-restore:hover {
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	.confirm-prompt {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-yellow, #eab308) 8%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-yellow, #eab308) 30%, transparent);
		border-radius: var(--radius);
		flex-wrap: wrap;
	}

	.confirm-text {
		font-size: 0.8em;
		color: var(--text-secondary);
		font-weight: 500;
	}

	.confirm-actions { display: flex; gap: var(--space-2); margin-left: auto; }

	.btn-cancel {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-cancel:hover:not(:disabled) {
		background: var(--bg-primary);
		color: var(--text-primary);
	}

	.btn-restore-confirm {
		padding: var(--space-1) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-restore-confirm:hover:not(:disabled) { filter: brightness(1.1); }

	.btn-restore-confirm:disabled,
	.btn-cancel:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
