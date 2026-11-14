<script lang="ts">
	import SegmentedControl from '$lib/components/common/SegmentedControl.svelte';
	import type { AudienceCounts, AudienceView } from '$lib/calendar/audience';
	let { value, counts, onchange }: {
		value: AudienceView;
		counts: AudienceCounts;
		onchange: (value: AudienceView) => void;
	} = $props();
</script>

<SegmentedControl label="Audience" {value} {onchange} fill options={[
	{ value: 'mine', label: 'Mine', count: counts.mine },
	{ value: 'agents', label: 'Agents', count: counts.agents },
	{ value: 'all', label: 'All', count: counts.all }
]} />
