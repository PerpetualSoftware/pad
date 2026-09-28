<script lang="ts">
	// Pending invitations addressed to you, with an Accept action per row: the
	// "Invitations" section of the TopBar "+" discovery surface (PLAN-3002 U4b /
	// TASK-3277, Dave's Q4 ruling). The server lists only invitations to your
	// VERIFIED email, so an unverified cloud account sees none.
	//
	// Accepting lands you in the workspace on an EPHEMERAL tab (Q5): an
	// invitation is somewhere you were sent, not somewhere you chose to keep.
	//
	// Renders NOTHING when the list is empty or the fetch failed, like
	// RecentlyDeletedWorkspaces beside it.
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { MyInvitation } from '$lib/types';

	interface Props {
		/** Re-fetched each time this turns true (the host surface opened). */
		active: boolean;
		/** Called once an accept has succeeded, before navigating. */
		onaccepted?: () => void;
	}

	let { active, onaccepted }: Props = $props();

	let invitations = $state<MyInvitation[]>([]);
	// Id being accepted: a double-click cannot fire two accepts.
	let acceptingId = $state<string | null>(null);
	// Guards list responses: an older fetch cannot re-surface an invitation
	// the post-accept refresh has already dropped.
	let seq = 0;

	async function load() {
		const mine = ++seq;
		try {
			const res = await api.members.listMyInvitations();
			if (mine === seq) invitations = res.invitations ?? [];
		} catch {
			if (mine === seq) invitations = [];
		}
	}

	$effect(() => {
		if (active) load();
	});

	async function accept(inv: MyInvitation) {
		if (acceptingId) return;
		acceptingId = inv.id;
		const isSameIdentity = authStore.identityFence();
		try {
			let slug = inv.workspace_slug;
			let owner = inv.workspace_owner_username;
			try {
				const res = await api.members.acceptMyInvitation(inv.id);
				slug = res.workspace_slug || slug;
				owner = res.owner_username || owner;
			} catch (err) {
				// A different account is signed in now; this failure is not theirs.
				if (!isSameIdentity()) return;
				toastStore.show(
					err instanceof Error && err.message ? err.message : `Couldn't accept the invitation to "${inv.workspace_name}"`,
					'error'
				);
				// It may have expired or been withdrawn; show the list as it is now.
				void load();
				return;
			}
			if (!isSameIdentity()) return;
			toastStore.show(`Joined "${inv.workspace_name}"`, 'success');
			onaccepted?.();
			// The membership is committed; everything below is best-effort. The
			// workspace list is reloaded so the new workspace is in it, then the
			// tab opens, then you land. A failed tab open still navigates.
			await workspaceStore.loadAll().catch(() => {});
			if (!isSameIdentity()) return;
			await tabsStore.open(slug, true).catch(() => {});
			if (!isSameIdentity()) return;
			// A workspace whose owner has no username has no path to land on.
			await goto(owner && slug ? `/${owner}/${slug}` : '/console');
		} finally {
			acceptingId = null;
		}
	}
</script>

{#if invitations.length > 0}
	<section class="invitations-section" aria-label="Invitations">
		<div class="invitations-header">
			<span class="invitations-title">Invitations</span>
			<span class="invitations-count">{invitations.length}</span>
		</div>
		<ul class="invitations-list">
			{#each invitations as inv (inv.id)}
				<li class="invitation-item" data-invitation-id={inv.id}>
					<span class="invitation-text">
						<span class="invitation-name" title={inv.workspace_name}>{inv.workspace_name}</span>
						<span class="invitation-meta">
							{inv.invited_by_name ? `${inv.invited_by_name} · ` : ''}{inv.role}
						</span>
					</span>
					<button
						type="button"
						class="accept-btn"
						onclick={() => accept(inv)}
						disabled={acceptingId !== null}
						aria-label={`Accept the invitation to ${inv.workspace_name}`}
					>
						{acceptingId === inv.id ? 'Joining…' : 'Accept'}
					</button>
				</li>
			{/each}
		</ul>
	</section>
{/if}

<style>
	.invitations-section { border-top: 1px solid var(--border); }
	.invitations-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-4);
		color: var(--text-muted);
		font-size: 0.8em;
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}
	.invitations-title { flex: 1; min-width: 0; }
	.invitations-count {
		flex-shrink: 0;
		padding: 0 var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		font-size: 0.9em;
	}
	.invitations-list { list-style: none; margin: 0; padding: 0; }
	.invitation-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-4);
		font-size: 0.9em;
	}
	.invitation-text {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
	}
	.invitation-name {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--text-primary);
	}
	.invitation-meta {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--text-muted);
		font-size: 0.85em;
	}
	.accept-btn {
		flex-shrink: 0;
		padding: var(--space-1) var(--space-2);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--accent-blue);
		cursor: pointer;
		font-size: 0.85em;
	}
	.accept-btn:hover:not(:disabled) { background: var(--bg-hover); }
	.accept-btn:disabled { opacity: 0.6; cursor: default; }
</style>
