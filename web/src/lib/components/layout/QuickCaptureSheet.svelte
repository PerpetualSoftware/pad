<!--
	QuickCaptureSheet — the mobile BottomNav's center "＋" action (PLAN-1694,
	TASK-1698). A self-contained capture flow: pick a collection, type a title,
	create. Deliberately does NOT reuse the Sidebar's quickAdd overlay
	(uiStore.requestQuickAdd) — that path only works while the Sidebar is
	mounted and renders a desktop-ish overlay (Codex review of PLAN-1694). The
	create logic mirrors Sidebar.svelte's submitQuickAdd so behaviour (default
	status, content template, navigate to the new item) stays consistent.

	Presents as a DockedSheet (anchored above the bottom nav) like the
	Workspace/You sheets, so the nav stays visible/tappable, the ＋ slot stays
	lit, and BottomNav's mutual-exclusion toggles work symmetrically. It was
	previously a full-screen common/BottomSheet that covered the nav and
	stacked on top of an open Search/Workspace/You surface (BUG-1765).
-->
<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { parseSchema, parseSettings, itemUrlId } from '$lib/types';
	import DockedSheet from '$lib/components/layout/DockedSheet.svelte';

	let {
		open,
		onclose,
		wsSlug,
		wsPrefix
	}: {
		open: boolean;
		onclose: () => void;
		wsSlug: string | undefined;
		wsPrefix: string;
	} = $props();

	// Agent/system collections (conventions, playbooks) are structured forms,
	// not quick-capture targets — mirror the Sidebar's regularCollections split.
	const agentSlugs = ['conventions', 'playbooks'];
	let collections = $derived(
		collectionStore.collections.filter((c) => !agentSlugs.includes(c.slug))
	);

	let selectedSlug = $state('');
	let title = $state('');
	let submitting = $state(false);

	// Default to tasks (or the first collection) when the sheet opens. Also
	// re-default if the remembered slug is no longer a valid collection (e.g.
	// the workspace changed since last open) — otherwise submit() would
	// silently no-op on a stale slug (Codex review, PLAN-1694).
	$effect(() => {
		if (!open || !collections.length) return;
		if (!selectedSlug || !collections.some((c) => c.slug === selectedSlug)) {
			selectedSlug = (collections.find((c) => c.slug === 'tasks') ?? collections[0]).slug;
		}
	});

	function close() {
		title = '';
		onclose();
	}

	async function submit() {
		if (!wsSlug || !selectedSlug || !title.trim() || submitting) return;
		const coll = collections.find((c) => c.slug === selectedSlug);
		if (!coll) return;
		submitting = true;
		const t = title.trim();
		try {
			const schema = parseSchema(coll);
			const settings = parseSettings(coll);
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find((f) => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			const item = await api.items.create(wsSlug, coll.slug, {
				title: t,
				content: settings.content_template || '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			title = '';
			onclose();
			uiStore.onNavigate();
			goto(`${wsPrefix}/${coll.slug}/${itemUrlId(item)}?new=1`);
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(
					planLimitMessage(err) + ' Upgrade to Pro',
					'error',
					6000,
					'/console/billing'
				);
			} else {
				toastStore.show(err?.message || 'Failed to create item', 'error');
			}
		} finally {
			submitting = false;
		}
	}

	function autofocus(node: HTMLElement) {
		requestAnimationFrame(() => node.focus());
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && !e.shiftKey) {
			e.preventDefault();
			submit();
		}
	}
</script>

<DockedSheet {open} onclose={close} label="Quick capture">
	<div class="capture">
		<h2 class="capture-heading">Quick capture</h2>
		<select class="capture-collection" bind:value={selectedSlug} aria-label="Collection">
			{#each collections as c (c.id)}
				<option value={c.slug}>{c.icon} {c.name}</option>
			{/each}
		</select>
		<textarea
			class="capture-title"
			bind:value={title}
			use:autofocus
			{onkeydown}
			placeholder="What needs doing?"
			rows="3"
		></textarea>
		<div class="capture-actions">
			<button class="capture-cancel" type="button" onclick={close}>Cancel</button>
			<button
				class="capture-submit"
				type="button"
				onclick={submit}
				disabled={!title.trim() || submitting}
			>
				{submitting ? 'Adding…' : 'Add'}
			</button>
		</div>
	</div>
</DockedSheet>

<style>
	.capture {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: 0 var(--space-5) var(--space-4);
	}
	.capture-heading {
		margin: 0;
		font-size: 1.05em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.capture-collection {
		appearance: none;
		background: var(--bg-primary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-2) var(--space-3);
		font-size: 0.95em;
		font-family: var(--font-ui);
	}
	.capture-title {
		background: var(--bg-primary);
		color: var(--text-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-3);
		font-size: 1em;
		font-family: var(--font-ui);
		resize: none;
		line-height: 1.4;
	}
	.capture-title:focus,
	.capture-collection:focus {
		outline: none;
		border-color: var(--accent-blue);
	}
	.capture-actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
	}
	.capture-cancel,
	.capture-submit {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-sm);
		font-size: 0.95em;
		font-weight: 600;
		cursor: pointer;
		border: 1px solid var(--border);
	}
	.capture-cancel {
		background: var(--bg-primary);
		color: var(--text-secondary);
	}
	.capture-submit {
		background: var(--accent-blue);
		color: #fff;
		border-color: var(--accent-blue);
	}
	.capture-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
