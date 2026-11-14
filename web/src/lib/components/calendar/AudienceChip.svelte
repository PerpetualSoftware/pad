<script lang="ts">
	import Chip from '../common/Chip.svelte';
	import AudienceGlyph from './AudienceGlyph.svelte';
	import { audienceChip, classify, type AudienceView } from '$lib/calendar/audience';
	import type { AgendaEntry } from '$lib/types';
	let { entry, view, isAgent = false }: { entry: AgendaEntry; view: AudienceView; isAgent?: boolean } = $props();
	const kind = $derived(audienceChip(classify(entry), view, isAgent));
</script>

{#if kind}<Chip size="sm" color={kind === 'agents' ? 'var(--accent-cyan)' : 'var(--accent-primary)'}><AudienceGlyph {kind} />{kind === 'agents' ? 'Agents' : 'You'}</Chip>{/if}
