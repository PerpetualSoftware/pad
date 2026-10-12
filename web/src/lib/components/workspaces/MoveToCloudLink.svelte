<script lang="ts">
	import { authStore } from '$lib/stores/auth.svelte';

	// The two entry points to the move-to-Cloud guide (TASK-3612): the
	// self-hosted settings page, and Cloud's New Workspace import. Never
	// inside a mobile app's web view: the guide as a whole steers to Pad
	// Cloud, which the app stores' anti-steering rules forbid there
	// (TASK-3299; lead ruling, day 92).
	interface Props {
		/** Which side this link sits on: shown only when the instance is that side. */
		on: 'self-hosted' | 'cloud';
		onnavigate?: () => void;
	}

	let { on, onnavigate }: Props = $props();

	const show = $derived(
		!authStore.nativeShell && (on === 'cloud' ? authStore.cloudMode : !authStore.cloudMode)
	);
</script>

{#if show}
	<p class="move-to-cloud">
		{on === 'cloud' ? 'Moving a workspace from a self-hosted Pad?' : 'Moving to Pad Cloud?'}
		<a href="/console/move-to-cloud" onclick={() => onnavigate?.()}>Follow the guide</a>.
	</p>
{/if}

<style>
	.move-to-cloud {
		font-size: 0.8em;
		color: var(--text-muted);
		margin: var(--space-1) 0 0;
	}
	/* A link inside running text must not rely on colour alone (axe link-in-text-block). */
	.move-to-cloud a {
		text-decoration: underline;
	}
</style>
