<!--
	Test double for `Lightbox` (TASK-2428). Records the props of every mount so
	a test can invoke a DESTROYED viewer's `onClose`, and renders a marker with
	the image it was opened on. Deliberately dumb: the real component's
	behaviour is covered against the real component elsewhere.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { lightboxStubCalls, type LightboxStubCall } from './lightboxStub';

	interface Props {
		images: { id: string }[];
		index?: number;
		wsSlug: string;
		onClose: () => void;
	}

	let { images, index = 0, wsSlug, onClose }: Props = $props();

	// Captured ONCE at mount, `untrack`ed like the real component's index seed:
	// the host remounts per open, so a recorded call belongs to exactly one
	// viewer instance — which is the whole point of the recording.
	const call: LightboxStubCall = untrack(() => ({ images, index, wsSlug, onClose }));
	lightboxStubCalls.push(call);
</script>

<div class="lightbox-stub" data-attachment-id={images[index]?.id ?? ''} data-ws={wsSlug}></div>
