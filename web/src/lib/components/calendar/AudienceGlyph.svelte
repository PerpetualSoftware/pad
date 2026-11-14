<script lang="ts">
	let { kind = 'agents' }: { kind?: 'agents' | 'you' } = $props();
</script>

<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
	{#if kind === 'agents'}
		<rect x="5" y="8" width="14" height="12" rx="3" /><path d="M12 4v4M9 12v3m6-3v3" /><circle cx="12" cy="3" r="1" />
	{:else}
		<circle cx="12" cy="7" r="4" /><path d="M5 21v-2a7 7 0 0 1 14 0v2" />
	{/if}
</svg>
