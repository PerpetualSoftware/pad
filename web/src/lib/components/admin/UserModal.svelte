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
	import { onMount, onDestroy } from 'svelte';
	import { tick } from 'svelte';
	import type { AdminUser } from '$lib/stores/admin.svelte';

	export type UserModalTab = 'overview' | 'workspaces' | 'activity' | 'settings';

	interface Props {
		user: AdminUser | null;
		open: boolean;
		initialTab?: UserModalTab;
		onClose: () => void;
	}

	let { user, open = $bindable(), initialTab, onClose }: Props = $props();

	// Current tab. Restored from URL hash on open so a reload keeps the
	// admin on the same tab; written back on tab change. Initialized to
	// a safe default and re-resolved in the open-side-effect so the
	// initialTab prop only captures the initial value via $effect (not
	// at first render, which would lock it).
	let activeTab = $state<UserModalTab>('overview');

	// Element refs for focus management.
	let modalEl = $state<HTMLDivElement | null>(null);
	let closeBtnEl = $state<HTMLButtonElement | null>(null);
	let previousFocus: HTMLElement | null = null;

	const TABS: { key: UserModalTab; label: string }[] = [
		{ key: 'overview', label: 'Overview' },
		{ key: 'workspaces', label: 'Workspaces' },
		{ key: 'activity', label: 'Activity' },
		{ key: 'settings', label: 'Settings & overrides' }
	];

	function parseHashTab(): UserModalTab | null {
		if (typeof window === 'undefined') return null;
		const m = window.location.hash.match(/tab=([a-z]+)/);
		if (!m) return null;
		const t = m[1];
		if (t === 'overview' || t === 'workspaces' || t === 'activity' || t === 'settings') return t;
		return null;
	}

	function writeHashTab(t: UserModalTab) {
		if (typeof window === 'undefined') return;
		// Preserve any other hash params; replace only tab=.
		const hash = window.location.hash.replace(/[#&]?tab=[^&]*/, '');
		const sep = hash.startsWith('#') ? '&' : '#';
		const next = hash + sep + 'tab=' + t;
		// History-quiet update — admins paste these all the time but
		// flipping tabs shouldn't litter back-button history.
		history.replaceState(null, '', window.location.pathname + window.location.search + next);
	}

	function selectTab(t: UserModalTab) {
		activeTab = t;
		writeHashTab(t);
	}

	// When the modal opens, capture the trigger element so we can restore
	// focus on close, hydrate the active tab from the URL hash (if present),
	// and pull focus into the modal.
	$effect(() => {
		if (open) {
			previousFocus = (document.activeElement as HTMLElement) ?? null;
			const fromHash = parseHashTab();
			if (fromHash) activeTab = fromHash;
			else activeTab = initialTab ?? 'overview';
			tick().then(() => closeBtnEl?.focus());
		} else if (previousFocus && document.contains(previousFocus)) {
			previousFocus.focus();
			previousFocus = null;
		}
	});

	// Keyboard handling — ESC closes; Tab is trapped within the modal so
	// focus can't escape into the table behind it.
	function handleKeydown(e: KeyboardEvent) {
		if (!open) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			closeModal();
			return;
		}
		if (e.key === 'Tab' && modalEl) {
			const focusables = modalEl.querySelectorAll<HTMLElement>(
				'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
			);
			if (focusables.length === 0) return;
			const first = focusables[0];
			const last = focusables[focusables.length - 1];
			const active = document.activeElement as HTMLElement | null;
			if (e.shiftKey && active === first) {
				e.preventDefault();
				last.focus();
			} else if (!e.shiftKey && active === last) {
				e.preventDefault();
				first.focus();
			}
		}
	}

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

	onMount(() => {});
	onDestroy(() => {});
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open && user}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="user-modal-backdrop" onclick={closeModal}>
		<div
			class="user-modal"
			bind:this={modalEl}
			role="dialog"
			tabindex="-1"
			aria-modal="true"
			aria-labelledby="user-modal-title"
			onclick={(e) => e.stopPropagation()}
		>
			<header class="user-modal-header">
				<h2 id="user-modal-title">
					{user.name || user.username || user.email}
					{#if user.disabled_at}<span class="badge disabled">disabled</span>{/if}
				</h2>
				<button
					bind:this={closeBtnEl}
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

			<div class="user-modal-body">
				{#each TABS as t (t.key)}
					<div
						role="tabpanel"
						tabindex="0"
						id="user-modal-panel-{t.key}"
						aria-labelledby="user-modal-tab-{t.key}"
						class="user-modal-panel"
						class:active={activeTab === t.key}
						hidden={activeTab !== t.key}
						data-testid="user-modal-panel-{t.key}"
					>
						{#if t.key === 'overview'}
							<p class="placeholder">Overview tab content arrives in TASK-1553.</p>
						{:else if t.key === 'workspaces'}
							<p class="placeholder">Workspaces tab content arrives in TASK-1552.</p>
						{:else if t.key === 'activity'}
							<p class="placeholder">Activity tab content arrives in TASK-1554.</p>
						{:else if t.key === 'settings'}
							<p class="placeholder">Settings & overrides tab content arrives in TASK-1551.</p>
						{/if}
					</div>
				{/each}
			</div>
		</div>
	</div>
{/if}

<style>
	.user-modal-backdrop {
		position: fixed;
		inset: 0;
		background: color-mix(in srgb, #000 50%, transparent);
		z-index: 1000;
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-4, 16px);
	}

	/* Modal width is a CSS variable so the Workspaces tab (T1552) can
	   widen to 960px once it has real per-workspace data without forking
	   the layout for every tab. */
	.user-modal {
		--user-modal-width: 720px;
		width: 100%;
		max-width: var(--user-modal-width);
		max-height: 90vh;
		display: flex;
		flex-direction: column;
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
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
		background: color-mix(in srgb, #ef4444 15%, transparent);
		color: #ef4444;
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
	.user-modal-panel.active {
		display: block;
	}
	.placeholder {
		color: var(--text-muted);
		font-style: italic;
		margin: 0;
	}
</style>
