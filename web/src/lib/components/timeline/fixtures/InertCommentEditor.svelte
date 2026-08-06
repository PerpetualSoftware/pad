<!--
	Test double for `CommentEditor` (TASK-2431).

	`ItemTimeline` mounts one as its composer, and it drags in Tiptap — which
	these tests neither exercise nor need. The timeline's own behaviour under
	test (which inline images may be handed to the viewer) is entirely in the
	rendered comment bodies, so the composer is replaced with a marker.

	Accepts and ignores every prop: a Svelte 5 rest-props catch-all, so adding a
	prop to the real component can never break this file.
-->
<script lang="ts">
	// eslint-disable-next-line @typescript-eslint/no-unused-vars
	let _props: Record<string, unknown> = $props();
</script>

<div class="comment-editor-stub"></div>
