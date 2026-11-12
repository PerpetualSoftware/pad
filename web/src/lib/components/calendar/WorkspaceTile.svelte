<script lang="ts">
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';

	/** A workspace as its colour tile and name: the cue for a list that crosses workspaces. */
	let { name }: { name: string } = $props();
</script>

<span class="ws"><i aria-hidden="true" style:background={avatarColor(name)}>{avatarInitial(name)}</i>{name}</span>

<style>
	.ws { display: inline-flex; align-items: center; gap: 5px; color: var(--text-secondary); }
	.ws i { width: 14px; height: 14px; border-radius: 4px; display: inline-grid; place-items: center; font-style: normal; font-size: 9px; font-weight: 800; color: #1a1a22; }
</style>
