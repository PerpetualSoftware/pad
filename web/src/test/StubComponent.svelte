<!-- Inert stand-in for a heavy child component in a route mount (BUG-3084).
     Rendering BoardView / ListView / PaneHost for real would drag their own
     stores, drag-and-drop and pane machinery into a suite whose subject is the
     PAGE's async handlers.

     It RECORDS its props, which is the seam the suite drives: every handler
     this page exposes to a view arrives as a prop (`onStatusChange`,
     `onArchiveColumn`, `onReorder`, …), so calling one through here runs the
     page's REAL handler with the page's real state. -->
<script lang="ts">
	const props: Record<string, unknown> = $props();
	const sink = (globalThis as { __stubProps?: Record<string, unknown>[] }).__stubProps;
	if (sink) sink.push(props);
</script>
