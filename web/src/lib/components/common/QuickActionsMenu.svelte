<script lang="ts">
	import type { QuickAction, Item, Collection } from '$lib/types';
	import { parseFields, formatItemRef, parseSettings } from '$lib/types';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';

	interface Props {
		actions: QuickAction[];
		item?: Item | null;
		collection: Collection;
		scope: 'item' | 'collection';
		wsSlug: string;
		canEdit?: boolean;
		onmanage?: () => void;
		oncollectionupdated?: (c: Collection) => void;
	}

	let {
		actions,
		item = null,
		collection,
		scope,
		wsSlug,
		canEdit = false,
		onmanage,
		oncollectionupdated
	}: Props = $props();

	let open = $state(false);
	let alignLeft = $state(false);
	let triggerEl = $state<HTMLButtonElement | null>(null);

	// ── Inline create-form state ─────────────────────────────────────────
	// `showCreateForm` collapses the action list and swaps in the inline
	// form when the user clicks "+ New quick action" in the footer.
	let showCreateForm = $state(false);
	let newLabel = $state('');
	let newPrompt = $state('');
	let newIcon = $state('');
	let saving = $state(false);

	let filtered = $derived(actions.filter((a) => a.scope === scope));

	// ── Viewport detection ────────────────────────────────────────────────
	// Track mobile viewport so we can swap the absolute-positioned popover
	// for a BottomSheet that never clips off-screen.
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	function resolvePrompt(action: QuickAction): string {
		let prompt = action.prompt;
		const fields = item ? parseFields(item) : {};

		const vars: Record<string, string> = {
			ref: item ? formatItemRef(item) ?? '' : '',
			title: item?.title ?? '',
			status: item ? String(fields['status'] ?? '') : '',
			priority: item ? String(fields['priority'] ?? '') : '',
			collection: collection.name,
			content: item?.content ? item.content.slice(0, 200) : '',
			fields: Object.entries(fields)
				.map(([k, v]) => `${k}: ${v}`)
				.join(', '),
			plan: item ? String(fields['plan'] ?? '') : '',
			phase: item ? String(fields['phase'] ?? fields['plan'] ?? '') : ''
		};

		for (const [key, value] of Object.entries(vars)) {
			prompt = prompt.replaceAll(`{${key}}`, value);
		}

		return prompt;
	}

	function copyToClipboard(text: string): boolean {
		// Try navigator.clipboard first
		if (navigator.clipboard?.writeText) {
			navigator.clipboard.writeText(text).catch(() => {});
			return true;
		}
		// Fallback: temporary textarea + execCommand
		const textarea = document.createElement('textarea');
		textarea.value = text;
		textarea.style.position = 'fixed';
		textarea.style.opacity = '0';
		document.body.appendChild(textarea);
		textarea.select();
		try {
			document.execCommand('copy');
			return true;
		} catch {
			return false;
		} finally {
			document.body.removeChild(textarea);
		}
	}

	function handleAction(action: QuickAction) {
		const resolved = resolvePrompt(action);
		if (copyToClipboard(resolved)) {
			toastStore.show('Copied to clipboard', 'success');
		} else {
			toastStore.show('Failed to copy to clipboard', 'error');
		}
		open = false;
	}

	function resetCreateForm() {
		showCreateForm = false;
		newLabel = '';
		newPrompt = '';
		newIcon = '';
	}

	function handleOpenCreateForm() {
		showCreateForm = true;
	}

	function handleManage() {
		open = false;
		resetCreateForm();
		onmanage?.();
	}

	async function handleSaveNewAction() {
		const label = newLabel.trim();
		const prompt = newPrompt.trim();
		if (!label || !prompt || saving) return;
		saving = true;
		try {
			const icon = newIcon.trim();
			const newAction: QuickAction = {
				label,
				prompt,
				scope,
				...(icon ? { icon } : {})
			};
			const settings = parseSettings(collection);
			const nextSettings = {
				...settings,
				quick_actions: [...(settings.quick_actions ?? []), newAction]
			};
			const updated = await api.collections.update(wsSlug, collection.slug, {
				settings: JSON.stringify(nextSettings)
			});
			toastStore.show('Saved', 'success');
			oncollectionupdated?.(updated);
			resetCreateForm();
			open = false;
		} catch (err) {
			toastStore.show(
				err instanceof Error ? err.message : 'Failed to save quick action',
				'error'
			);
		} finally {
			saving = false;
		}
	}

	function handleTriggerClick(e: MouseEvent) {
		e.stopPropagation();
		const nextOpen = !open;
		// Only compute alignment when opening on desktop; the mobile branch
		// renders a BottomSheet which doesn't need trigger-relative positioning.
		if (nextOpen && !isMobile && triggerEl) {
			const rect = triggerEl.getBoundingClientRect();
			// If the trigger is too close to the left edge, the default
			// right-anchored 200px dropdown would clip — switch to left-anchored.
			alignLeft = rect.left < 220;
		}
		if (!nextOpen) {
			// Closing via trigger toggles the form off too so the next open
			// returns to the action list.
			resetCreateForm();
		}
		open = nextOpen;
	}

	function handleWindowClick(e: MouseEvent) {
		// On mobile the BottomSheet owns dismissal (backdrop tap, Escape,
		// swipe-down) — skip the outside-click handler so it doesn't race.
		if (isMobile) return;
		const target = e.target as HTMLElement;
		if (!target) return;
		// The EmojiPickerButton portals its dropdown to document.body (or the
		// nearest <dialog>); clicks inside the portal (.epb-dropdown) or on
		// the emoji trigger itself (.emoji-picker-button) should NOT close
		// the menu, or the in-progress emoji selection is lost before the
		// bound value can update.
		if (target.closest('.epb-dropdown') || target.closest('.emoji-picker-button')) return;
		if (!target.closest('.quick-actions-menu')) {
			open = false;
			resetCreateForm();
		}
	}

	function handleBottomSheetClose() {
		open = false;
		resetCreateForm();
	}
</script>

<svelte:window onclick={handleWindowClick} />

{#snippet createForm()}
	<div class="create-form">
		<div class="qa-row">
			<EmojiPickerButton bind:value={newIcon} placeholder="+" size="sm" />
			<input
				class="qa-label-input"
				type="text"
				placeholder="Action label"
				bind:value={newLabel}
			/>
		</div>
		<textarea
			class="qa-prompt-input"
			placeholder="/pad ..."
			rows="3"
			bind:value={newPrompt}
		></textarea>
		<div class="qa-help">
			Template variables: {'{ref}'} {'{title}'} {'{status}'} {'{priority}'} {'{collection}'} {'{content}'} {'{fields}'}
		</div>
		<div class="qa-actions">
			<button class="qa-btn qa-btn-cancel" type="button" onclick={resetCreateForm}>
				Cancel
			</button>
			<button
				class="qa-btn qa-btn-save"
				type="button"
				onclick={handleSaveNewAction}
				disabled={!newLabel.trim() || !newPrompt.trim() || saving}
			>
				{saving ? 'Saving...' : 'Save'}
			</button>
		</div>
	</div>
{/snippet}

{#snippet actionList()}
	{#if showCreateForm}
		{@render createForm()}
	{:else}
		{#each filtered as action (action.label)}
			<button class="action-item" onclick={() => handleAction(action)}>
				{#if action.icon}
					<span class="action-icon">{action.icon}</span>
				{/if}
				<span class="action-label">{action.label}</span>
			</button>
		{/each}
		<div class="dropdown-tagline">Copy a prompt to your agent</div>
		{#if canEdit}
			<div class="footer-divider"></div>
			<button class="action-item footer-row" type="button" onclick={handleOpenCreateForm}>
				<span class="action-icon">+</span>
				<span class="action-label">New quick action</span>
			</button>
			<button class="action-item footer-row" type="button" onclick={handleManage}>
				<span class="action-icon">&#9881;</span>
				<span class="action-label">Manage actions</span>
			</button>
		{/if}
	{/if}
{/snippet}

{#if filtered.length > 0 || canEdit}
	<div class="quick-actions-menu">
		<button
			bind:this={triggerEl}
			class="trigger-btn"
			onclick={handleTriggerClick}
			title="Quick actions"
		>
			&#9889;
		</button>

		{#if isMobile}
			<BottomSheet {open} onclose={handleBottomSheetClose} title="Quick actions">
				{@render actionList()}
			</BottomSheet>
		{:else if open}
			<div class="dropdown" class:align-left={alignLeft}>
				{@render actionList()}
			</div>
		{/if}
	</div>
{/if}

<style>
	.quick-actions-menu {
		position: relative;
		display: inline-block;
	}

	.trigger-btn {
		padding: 2px var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
	}

	.trigger-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.dropdown {
		position: absolute;
		top: 100%;
		right: 0;
		margin-top: var(--space-1);
		min-width: 240px;
		/* Defensive cap so the popover can never overflow the viewport
		   horizontally, even if trigger placement or zoom produces an
		   edge case we didn't anticipate. */
		max-width: calc(100vw - var(--space-4));
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
		z-index: 50;
		padding: var(--space-1) 0;
	}

	/* When the trigger sits near the viewport's left edge, flip to
	   left-anchored so the dropdown opens rightward and stays on-screen. */
	.dropdown.align-left {
		right: auto;
		left: 0;
	}

	.action-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		color: var(--text-primary);
		font-size: 0.85em;
		cursor: pointer;
		text-align: left;
		transition: background 0.1s;
	}

	.action-item:hover {
		background: var(--bg-tertiary);
	}

	.action-icon {
		flex-shrink: 0;
	}

	.action-label {
		flex: 1;
		min-width: 0;
	}

	.dropdown-tagline {
		padding: var(--space-2) var(--space-3);
		font-size: 0.72em;
		color: var(--text-muted);
		border-top: 1px solid var(--border);
		text-align: center;
	}

	/* ── Footer rows (gated by canEdit) ─────────────────────────────── */

	.footer-divider {
		height: 1px;
		background: var(--border);
		margin: var(--space-1) 0;
	}

	.footer-row {
		color: var(--text-secondary);
	}

	.footer-row:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}

	/* ── Inline create form ─────────────────────────────────────────── */

	.create-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
	}

	.qa-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.qa-label-input {
		flex: 1;
		min-width: 0;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.qa-label-input:hover {
		border-color: var(--border);
	}

	.qa-label-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.qa-prompt-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-primary);
		resize: vertical;
		min-height: 60px;
	}

	.qa-prompt-input:hover {
		border-color: var(--border);
	}

	.qa-prompt-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.qa-help {
		font-size: 0.7em;
		color: var(--text-muted);
		line-height: 1.4;
		word-break: break-word;
	}

	.qa-actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-top: var(--space-1);
	}

	.qa-btn {
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.8em;
		cursor: pointer;
		border: 1px solid var(--border);
	}

	.qa-btn-cancel {
		background: var(--bg-tertiary);
		color: var(--text-secondary);
	}

	.qa-btn-cancel:hover {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}

	.qa-btn-save {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
		font-weight: 500;
	}

	.qa-btn-save:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.qa-btn-save:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
