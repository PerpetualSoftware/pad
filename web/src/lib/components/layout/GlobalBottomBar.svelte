<!--
	GlobalBottomBar — the phone bar OUTSIDE the workspace layout: the You pages,
	the workspace picker and a standalone /messages (TASK-4315, TASK-4546). It is
	the same BottomNav the workspace layout mounts, so the slots never differ.
	The root layout mounts it only for non-auth, non-share pages, outside every
	`inert` wrapper, so it stays live under the Chat sheet.
-->
<script lang="ts">
	import BottomNav from './BottomNav.svelte';
</script>
<BottomNav />
