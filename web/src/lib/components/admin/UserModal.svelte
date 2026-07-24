<!--
  Admin user detail modal — the new home for per-user deep view that
  replaces the inline-expand pattern on the user table.

  This task (T1550) lands ONLY the shell: open/close, tab nav, ESC + click-
  outside dismiss, focus trap basics, and empty placeholders. Tab content
  arrives across T1551 (Settings & overrides — lifts the inline-expand
  form), T1552 (Workspaces), T1553 (Overview), T1554 (Activity). T1555
  deletes the inline-expand DOM from the host page.

  During T1550–T1554 the inline-expand still works in parallel with the
  modal; that's the explicit "broken state" the plan accepts to keep PRs
  small. Documented in the on-click handler comment in the host page.

  PLAN-1542 / TASK-1550.
-->
<script lang="ts">
	import type { AdminUser } from '$lib/stores/admin.svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import UserSettingsForm from './UserSettingsForm.svelte';
	import UserWorkspacesTab from './UserWorkspacesTab.svelte';
	import UserOverviewTab from './UserOverviewTab.svelte';
	import UserActivityTab from './UserActivityTab.svelte';

	export type UserModalTab = 'overview' | 'workspaces' | 'activity' | 'settings';

	interface Props {
		user: AdminUser | null;
		open: boolean;
		initialTab?: UserModalTab;
		onClose: () => void;
		/** Bubbled from the Settings tab when a save completes — parent
		 *  uses it to keep the table row in sync. PLAN-1542 / TASK-1551. */
		onUserUpdated?: (updated: AdminUser) => void;
	}

	let { user, open = $bindable(), initialTab, onClose, onUserUpdated }: Props = $props();

	// Current tab. Restored from URL hash on open so a reload keeps the
	// admin on the same tab; written back on tab change. Initialized to
	// a safe default and re-resolved in the open-side-effect so the
	// initialTab prop only captures the initial value via $effect (not
	// at first render, which would lock it).
	let activeTab = $state<UserModalTab>('overview');

	const TABS: { key: UserModalTab; label: string }[] = [
		{ key: 'overview', label: 'Overview' },
		{ key: 'workspaces', label: 'Workspaces' },
		{ key: 'activity', label: 'Activity' },
		{ key: 'settings', label: 'Settings & overrides' }
	];

	// Hash round-trip uses URLSearchParams so other hash params (used
	// elsewhere on the admin page or by other tabs) survive. Previous
	// regex-replace approach corrupted hashes where tab= was the first
	// key but other params followed (Codex review on PR #605).
	function readHashParams(): URLSearchParams {
		if (typeof window === 'undefined') return new URLSearchParams();
		return new URLSearchParams(window.location.hash.replace(/^#/, ''));
	}

	function parseHashTab(): UserModalTab | null {
		const t = readHashParams().get('tab');
		if (t === 'overview' || t === 'workspaces' || t === 'activity' || t === 'settings') return t;
		return null;
	}

	function writeHashTab(t: UserModalTab) {
		if (typeof window === 'undefined') return;
		const params = readHashParams();
		params.set('tab', t);
		const next = '#' + params.toString();
		history.replaceState(null, '', window.location.pathname + window.location.search + next);
	}

	function selectTab(t: UserModalTab) {
		activeTab = t;
		writeHashTab(t);
	}

	// Open transition tracker — hydrate the active tab on the open=false → true
	// transition only, not whenever initialTab or any other reactive value
	// changes mid-open (Codex review on PR #605). Focus save/restore, Escape,
	// and the Tab focus-trap are now handled by the native <dialog> in <Modal>.
	// `wasOpen` is a plain let (not $state) — edge-detection only — and this
	// effect writes `activeTab` without reading it, so it can't self-invalidate
	// (CONVE-1688).
	let wasOpen = false;
	$effect(() => {
		if (open && !wasOpen) {
			const fromHash = parseHashTab();
			activeTab = fromHash ?? initialTab ?? 'overview';
			wasOpen = true;
		} else if (!open && wasOpen) {
			wasOpen = false;
		}
	});

	function closeModal() {
		open = false;
		onClose();
	}

	// Body scroll-lock while the modal is open so the table behind doesn't
	// scroll on wheel.
	$effect(() => {
		if (typeof document === 'undefined') return;
		if (open) {
			const prev = document.body.style.overflow;
			document.body.style.overflow = 'hidden';
			return () => {
				document.body.style.overflow = prev;
			};
		}
	});

</script>

<Modal
	open={open && !!user}
	onclose={closeModal}
	labelledby="user-modal-title"
	placement="center"
	maxWidth="720px"
	--modal-bg="var(--bg-primary)"
	--modal-radius="var(--radius)"
	--modal-shadow="0 8px 32px rgba(0, 0, 0, 0.4)"
>
	{#if user}
		<div class="user-modal">
			<header class="user-modal-header">
				<h2 id="user-modal-title">
					{user.name || user.username || user.email}
					{#if user.disabled_at}<span class="badge disabled">disabled</span>{/if}
				</h2>
				<button
					type="button"
					class="user-modal-close"
					aria-label="Close"
					onclick={closeModal}>×</button
				>
			</header>

			<div class="user-modal-tabs" role="tablist" aria-label="User detail sections">
				{#each TABS as t (t.key)}
					<button
						type="button"
						role="tab"
						class="user-modal-tab"
						class:active={activeTab === t.key}
						aria-selected={activeTab === t.key}
						aria-controls="user-modal-panel-{t.key}"
						id="user-modal-tab-{t.key}"
						onclick={() => selectTab(t.key)}
					>
						{t.label}
					</button>
				{/each}
			</div>

			<!-- Lazy-fetch tabs (Overview / Workspaces / Activity) mount
			     only when active so their fetches fire on first
			     selection. Settings is kept mounted with a hidden toggle
			     so unsaved overrides / typed-disable text aren't lost
			     if an admin briefly switches to Workspaces and back.
			     UserSettingsForm has no fetch — just synchronous state —
			     so keeping it alive is cheap. Its hydration $effect is
			     gated on user.id so the original loop stays fixed. -->
			<div
				class="user-modal-body"
				role="tabpanel"
				tabindex="0"
				id="user-modal-panel-{activeTab}"
				aria-labelledby="user-modal-tab-{activeTab}"
				data-testid="user-modal-panel-{activeTab}"
			>
				{#if activeTab === 'overview'}
					<UserOverviewTab {user} active={true} />
				{:else if activeTab === 'workspaces'}
					<UserWorkspacesTab {user} active={true} />
				{:else if activeTab === 'activity'}
					<UserActivityTab {user} active={true} />
				{/if}
				<div hidden={activeTab !== 'settings'}>
					<UserSettingsForm {user} {onUserUpdated} />
				</div>
			</div>
		</div>
	{/if}
</Modal>

<style>
	/* Fills the <Modal> dialog box (which owns the surface chrome / max-height).
	   Kept as a flex column so the header/tabs pin and the body scrolls. */
	.user-modal {
		width: 100%;
		flex: 1;
		min-height: 0;
		display: flex;
		flex-direction: column;
	}

	.user-modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.user-modal-header h2 {
		margin: 0;
		font-size: 1.1rem;
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.badge.disabled {
		font-size: 0.7rem;
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		background: color-mix(in srgb, var(--accent-red) 15%, transparent);
		color: var(--accent-red);
	}
	.user-modal-close {
		background: transparent;
		border: 0;
		font-size: 1.5rem;
		line-height: 1;
		color: var(--text-muted);
		cursor: pointer;
		padding: 4px 8px;
		border-radius: var(--radius-sm);
	}
	.user-modal-close:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.user-modal-tabs {
		display: flex;
		gap: 2px;
		border-bottom: 1px solid var(--border);
		padding: 0 var(--space-3);
		flex-wrap: wrap;
	}
	.user-modal-tab {
		background: transparent;
		border: 0;
		padding: var(--space-2) var(--space-3);
		font: inherit;
		color: var(--text-muted);
		cursor: pointer;
		border-bottom: 2px solid transparent;
		border-radius: 0;
	}
	.user-modal-tab:hover {
		color: var(--text-primary);
	}
	.user-modal-tab.active {
		color: var(--accent-blue);
		border-bottom-color: var(--accent-blue);
	}
	.user-modal-tab:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.user-modal-body {
		overflow-y: auto;
		padding: var(--space-4);
		flex: 1;
	}
	/* .user-modal-panel.active rule removed — tabs are now mount-on-active,
	   so no panel needs an "active" toggle class. */
	/* .placeholder rule removed — all four tabs now render real content
	   as of TASK-1554. No remaining users of the class. */
</style>
