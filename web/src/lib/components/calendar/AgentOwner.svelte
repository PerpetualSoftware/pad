<script lang="ts">
	import { classify } from '$lib/calendar/audience';
	import type { AgendaEntry } from '$lib/types';
	import AudienceGlyph from './AudienceGlyph.svelte';
	let { entry }: { entry: AgendaEntry } = $props();
</script>

{#if classify(entry) === 'agents' && entry.agent_role_name}
	<span class="owner"><AudienceGlyph />{entry.agent_role_name}</span>
{/if}

<style>
	.owner { display: inline-flex; align-items: center; gap: 4px; color: var(--text-secondary); font-size: 12px; font-weight: 400; margin-left: 4px; }
</style>
