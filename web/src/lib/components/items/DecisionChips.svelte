<script lang="ts">
	// Attention chips on the item page (TASK-3118). Self-fetching rather than
	// read off the item object: the item page is also fed by list and delta
	// projections that do not carry `decisions`, so a chip bound to the item
	// would vanish whenever one of those replaced it.
	//
	// Renders NOTHING when there are no current answers — which is every item
	// on an instance with no decision provider, so that case is identical to
	// before this component existed.
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { attentionChips, type AttentionChip } from '$lib/decisions/attentionChips';
	import { conventionChips, type ConventionChip } from '$lib/decisions/conventionChips';

	interface Props {
		wsSlug: string;
		itemRef: string;
		itemId: string;
	}
	let { wsSlug, itemRef, itemId }: Props = $props();

	let chips = $state<AttentionChip[]>([]);
	// Convention chips (TASK-3119 U1b): only "Possibly breaks CONVE-N", never a
	// "complies" state; none means nothing.
	let convChips = $state<ConventionChip[]>([]);
	// Monotonic request token: a response for an item the page has since
	// switched away from must not paint over the current item's chips.
	let latest = 0;

	$effect(() => {
		const ws = wsSlug;
		const ref = itemRef;
		void itemId; // re-fetch on identity change, not only on ref text
		const token = ++latest;
		// `latest` answers "is this still the item on screen", which a sign-out
		// or an account swap does not change — the answer is computed from what
		// the CALLER may see, so it must not paint for the next one (BUG-3130).
		const isSameIdentity = authStore.identityFence();
		chips = [];
		convChips = [];
		if (!ws || !ref) return;
		api.items
			.decisions(ws, ref)
			.then((res) => {
				if (token === latest && isSameIdentity()) {
					chips = attentionChips(res.decisions);
					convChips = conventionChips(res.decisions, ws);
				}
			})
			.catch((err) => {
				// Advisory surface: a failed read shows no chips rather than an
				// error, and is logged so it is not invisible.
				if (token === latest) console.warn('decision chips: read failed', err);
			});
	});
</script>

<!--
	data-sveltekit-reload: /-/r/{ws}/{ref} is a SERVER redirect route with no
	client route behind it, so the SvelteKit router must not take the click
	(it matches [username]=- / [workspace]=r and shows "Workspace not found").
-->
{#if convChips.length > 0}
	<div class="decision-chips" aria-label="Convention checks">
		{#each convChips as chip (chip.ref)}
			<a
				class="decision-chip convention-chip"
				href={chip.href}
				data-sveltekit-reload
				title="{chip.label}: judged {chip.percent}% likely from the item's text, links and recent comments. Advisory only; a missing chip is not a verdict."
			>
				<span class="decision-chip-label">{chip.label}</span>
			</a>
		{/each}
	</div>
{/if}

{#if chips.length > 0}
	<div class="decision-chips" aria-label="Attention signals">
		{#each chips as chip (chip.key)}
			<span
				class="decision-chip"
				class:flagged={chip.flagged}
				title="{chip.label}: {chip.percent}% likely, judged from the item's text and recent comments"
			>
				<span class="decision-chip-label">{chip.label}</span>
				<span class="decision-chip-value">{chip.percent}%</span>
			</span>
		{/each}
	</div>
{/if}

<style>
	.decision-chips {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		margin-bottom: var(--space-2);
	}
	.decision-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: 1px 8px;
		border: 1px solid var(--border);
		border-radius: 999px;
		font-size: 0.75em;
		color: var(--text-muted);
		background: transparent;
	}
	.decision-chip.flagged {
		color: var(--text-primary);
		border-color: var(--accent-amber, var(--accent-blue));
		background: color-mix(in srgb, var(--accent-amber, var(--accent-blue)) 12%, transparent);
	}
	.convention-chip {
		color: var(--text-primary);
		border-color: var(--accent-amber, var(--accent-blue));
		text-decoration: none;
	}
	.convention-chip:hover {
		background: color-mix(in srgb, var(--accent-amber, var(--accent-blue)) 12%, transparent);
	}
	.decision-chip-value {
		font-variant-numeric: tabular-nums;
	}
</style>
