<script lang="ts">
 let { label }: { label: string | null } = $props();
</script>

{#if label}
 <span class="snooze">
  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>
  <span>{label}</span>
 </span>
{/if}

<style>
 .snooze { display: inline-flex; align-items: baseline; gap: 4px; max-width: 100%; font-size: 11.5px; font-weight: 400; color: var(--text-muted); white-space: normal; }
 svg { flex: 0 0 12px; align-self: center; }
</style>
