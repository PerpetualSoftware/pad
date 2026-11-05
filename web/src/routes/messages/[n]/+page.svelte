<script lang="ts">
	import { page } from '$app/state';
	import ChatThreadView from '$lib/components/chat/ChatThreadView.svelte';

	let raw = $derived(page.params.n ?? '');
	let n = $derived(/^[1-9][0-9]*$/.test(raw) ? Number(raw) : null);
</script>

<svelte:head>
	<title>Chat · Pad</title>
</svelte:head>

<ChatThreadView {n} />
