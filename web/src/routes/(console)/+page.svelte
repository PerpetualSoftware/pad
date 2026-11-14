<script lang="ts">
	import DesktopHome from '$lib/components/home/DesktopHome.svelte';
	import { onMount, onDestroy } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { browser } from '$app/env';
	import { uiStore } from '$lib/stores/ui.svelte';

	onMount(() => {
		// Honor the openCreate=1 query param staged by /?openCreate=1's
		// redirect (IDEA-1516 §1). Open the modal and scrub the param
		// from the URL so a subsequent refresh doesn't re-open it.
		// Lives in onMount (not an $effect) so it runs exactly once
		// per page mount — modal-toggle is a side-effect, not reactive
		// state derived from the URL.
		if (browser && page.url.searchParams.get('openCreate') === '1') {
			uiStore.openCreateWorkspace();
			const cleaned = new URL(page.url.href);
			cleaned.searchParams.delete('openCreate');
			void goto(cleaned.pathname + cleaned.search + cleaned.hash, {
				replaceState: true,
				reset: false,
			});
		}

	});

	// The Q2 highlight lasts only while this page is up.
	onDestroy(() => uiStore.clearAddWorkspaceHighlight());
</script>

<DesktopHome />
