<!-- Binds Button's and IconButton's `element` so a test can read the node back. -->
<script lang="ts">
	import Button from '../Button.svelte';
	import IconButton from '../IconButton.svelte';

	let { onbound }: { onbound: (button: HTMLElement | undefined, icon: HTMLElement | undefined) => void } = $props();
	let button = $state<HTMLButtonElement>();
	let icon = $state<HTMLButtonElement>();
	$effect(() => onbound(button, icon));
</script>

<Button bind:element={button} class="bound">Bound</Button>
<IconButton bind:element={icon} label="Bound icon">x</IconButton>
