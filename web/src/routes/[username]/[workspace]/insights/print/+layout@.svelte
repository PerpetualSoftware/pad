<script lang="ts">
	// Layout reset for the print/exec-report route.
	//
	// The `@` (bare) suffix re-parents this segment to the ROOT layout,
	// skipping the workspace `+layout.svelte` — so the report does NOT inherit
	// the workspace ConnectBanner or its SSE/sync wiring. The root layout still
	// renders the app shell (sidebar + topbar) around `{@render children()}`;
	// the print page itself hides that chrome via `@media print` :global() rules
	// so a Save-as-PDF produces only the report. On screen the chrome remains,
	// which keeps the back link / navigation usable while previewing.
	let { children } = $props();
</script>

{@render children()}
