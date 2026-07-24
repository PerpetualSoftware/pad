<script lang="ts">
	import type { QuickAction, Item, Collection } from '$lib/types';
	import { parseFields, formatItemRef, parseSettings } from '$lib/types';
	import { api, isConflictOrNotFound } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
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
	let triggerEl = $state<HTMLButtonElement>();

	// ── Inline create-form state ─────────────────────────────────────────
	// `showCreateForm` collapses the action list and swaps in the inline
	// form when the user clicks "+ New quick action" in the footer.
	//
	// The BUG-2281 stopPropagation workaround (open/cancel clicks racing the
	// window click-outside handler on a detached target) is retired: the Menu
	// primitive's outside-click is pointerdown-based and fires BEFORE a row
	// click mutates state, so the detach hazard is structurally gone.
	let showCreateForm = $state(false);
	let newLabel = $state('');
	let newPrompt = $state('');
	let newIcon = $state('');
	let saving = $state(false);

	let filtered = $derived(actions.filter((a) => a.scope === scope));

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

	function handleManage() {
		// Recheck `canEdit` at dispatch time (mirrors handleSaveNewAction): if it
		// flips false while the menu is open — e.g. the master-freeze passing
		// canEdit=false once this side becomes the peeking preview (BUG-2263) — refuse
		// to open the collection-settings editor. The trigger itself unmounts on the
		// same flip (`{#if canEdit}`); this guards the render→click race.
		if (!canEdit) return;
		open = false;
		resetCreateForm();
		onmanage?.();
	}

	async function handleSaveNewAction() {
		// Recheck `canEdit` at dispatch time: if it flips false while the create
		// form is open (e.g. the PLAN-2154 master-freeze passing canEdit=false
		// while peeking, TASK-2172), refuse the api.collections.update. The form
		// itself unmounts on the same flip (see `{#if showCreateForm && canEdit}`).
		if (!canEdit) return;
		const label = newLabel.trim();
		const prompt = newPrompt.trim();
		if (!label || !prompt || saving) return;
		saving = true;
		// Capture workspace + collection identity BEFORE any await. This
		// component is reused across items/collections without a guaranteed
		// remount, so reading live props after the await could fetch/update the
		// WRONG collection on a mid-save navigation (BUG-2265 switch-safety).
		const ws = wsSlug;
		const baseCollection = collection;
		const slug = baseCollection.slug;
		try {
			const icon = newIcon.trim();
			const newAction: QuickAction = {
				label,
				prompt,
				scope,
				...(icon ? { icon } : {})
			};

			// BUG-2265: append onto a base snapshot AND round-trip its
			// updated_at so the server rejects a stale write instead of
			// clobbering a sibling ItemDetail's concurrent settings change.
			// `appendOnto` re-derives the full settings from a FRESH base
			// each time, so a 409-triggered refetch+retry re-applies our new
			// action onto whatever the other writer just saved (no silent
			// loss) rather than replaying our stale local snapshot.
			const appendOnto = (base: Collection): string => {
				const s = parseSettings(base);
				return JSON.stringify({
					...s,
					quick_actions: [...(s.quick_actions ?? []), newAction]
				});
			};

			let updated: Collection;
			try {
				updated = await api.collections.update(ws, slug, {
					settings: appendOnto(baseCollection),
					expected_updated_at: baseCollection.updated_at
				});
			} catch (err) {
				// A concurrent change can defeat our slug-targeted write two ways:
				// a 409 update_conflict (settings changed) OR a 404 not_found (a
				// RENAME killed the slug). Recover from BOTH (BUG-2265 Pattern C):
				// resolve the collection by its STABLE id, re-append onto its
				// fresh settings, and retry ONCE. Surface only if it's truly gone.
				if (!isConflictOrNotFound(err)) throw err;
				const list = await api.collections.list(ws);
				const fresh = list.find((c) => c.id === baseCollection.id);
				if (!fresh) throw err; // collection gone (deleted) — surface it
				updated = await api.collections.update(ws, fresh.slug, {
					settings: appendOnto(fresh),
					expected_updated_at: fresh.updated_at
				});
			}
			// Only propagate the result if this component still represents the
			// SAME collection it started with — compared by STABLE id so a
			// concurrent rename (slug changed, same collection) still
			// propagates, while a genuine switch to a DIFFERENT collection
			// (different id) is dropped. On a reused route (no guaranteed
			// remount) `oncollectionupdated` is the live (navigated) page's
			// callback — feeding it our old response would assign stale data to
			// the wrong page (Codex switch-safety).
			if (wsSlug !== ws || collection?.id !== baseCollection.id) return;
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

	function handleTriggerClick() {
		const nextOpen = !open;
		if (nextOpen && triggerEl) {
			const rect = triggerEl.getBoundingClientRect();
			// If the trigger is too close to the left edge, the default
			// right-anchored dropdown would clip — switch to left-anchored.
			// (Only matters for the desktop anchored panel; the mobile
			// BottomSheet ignores alignment, so computing it is harmless.)
			alignLeft = rect.left < 220;
		}
		if (!nextOpen) {
			// Closing via trigger toggles the form off too so the next open
			// returns to the action list.
			resetCreateForm();
		}
		open = nextOpen;
	}

	function closeMenu() {
		open = false;
		resetCreateForm();
	}

	// The EmojiPickerButton portals its dropdown to document.body (or the
	// nearest <dialog>), so it is NOT inside the Menu panel's DOM — pass its
	// containers as outside-click exemptions or the in-progress emoji
	// selection closes the whole menu before the bound value can update.
	// Queried from document because of the portal.
	function emojiPickerContainers(): (Element | null | undefined)[] {
		return [
			...document.querySelectorAll('.epb-dropdown'),
			...document.querySelectorAll('.emoji-picker-button')
		];
	}
</script>

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
	{#if showCreateForm && canEdit}
		{@render createForm()}
	{:else}
		{#each filtered as action (action.label)}
			<MenuItem icon={action.icon} onclick={() => handleAction(action)}>
				{action.label}
			</MenuItem>
		{/each}
		<div class="dropdown-tagline">Copy a prompt to your agent</div>
		{#if canEdit}
			<div class="footer-divider"></div>
			<MenuItem icon="+" onclick={() => (showCreateForm = true)}>New quick action</MenuItem>
			<MenuItem icon="⚙" onclick={handleManage}>Manage actions</MenuItem>
		{/if}
	{/if}
{/snippet}

{#if filtered.length > 0 || canEdit}
	<div class="quick-actions-menu">
		<button
			bind:this={triggerEl}
			class="trigger-btn"
			aria-haspopup="menu"
			aria-expanded={open}
			onclick={handleTriggerClick}
			title="Quick actions"
		>
			&#9889;
		</button>

		<Menu
			{open}
			onclose={closeMenu}
			trigger={triggerEl}
			align={alignLeft ? 'left' : 'right'}
			sheetOnMobile
			sheetTitle="Quick actions"
			ariaLabel="Quick actions"
			exempt={emojiPickerContainers}
		>
			<div class="qa-body">
				{@render actionList()}
			</div>
		</Menu>
	</div>
{/if}

<style>
	.quick-actions-menu {
		/* Anchor for Menu's anchored mode. */
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

	/* Sizes the slotted content (Menu's panel is min-width 180px on its
	   own — too narrow for the inline create form). The viewport cap is
	   defensive: the popover should never overflow horizontally even if
	   trigger placement or zoom produces an edge case we didn't anticipate. */
	.qa-body {
		min-width: 230px;
		max-width: calc(100vw - var(--space-4));
	}

	.dropdown-tagline {
		padding: var(--space-2) var(--space-3);
		font-size: 0.72em;
		color: var(--text-muted);
		border-top: 1px solid var(--border);
		text-align: center;
	}

	/* ── Footer (gated by canEdit) ──────────────────────────────────── */

	.footer-divider {
		height: 1px;
		background: var(--border);
		margin: var(--space-1) 0;
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
