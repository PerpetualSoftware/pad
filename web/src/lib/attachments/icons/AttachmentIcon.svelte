<!--
	AttachmentIcon — the Svelte render path for the attachment file-type icon
	set (PLAN-2392 DR-3b). Imperative consumers (the editor's chip NodeView)
	call `iconSvg()` from ./index instead; both read the same path table, so
	the two surfaces can't drift.

	The icon is decorative — `aria-hidden` comes from ICON_SVG_ATTRS and every
	call site labels the attachment with its filename in real text.
-->
<script lang="ts">
	import {
		ATTACHMENT_ICON_PATHS,
		GENERIC_ICON_ID,
		ICON_SVG_ATTRS,
		iconSize,
		isAttachmentIconId,
	} from './index';

	let {
		id,
		size = undefined,
	}: {
		/** An icon id — usually `iconForAttachment(mime, filename)`. Unknown ids render the generic file. */
		id: string;
		/** CSS length, or a number of pixels. Defaults to `1em` so it scales with the surface's type. */
		size?: number | string;
	} = $props();

	const iconId = $derived(isAttachmentIconId(id) ? id : GENERIC_ICON_ID);
	const box = $derived(iconSize(size));
</script>

<svg
	xmlns="http://www.w3.org/2000/svg"
	width={box}
	height={box}
	style="display:block"
	{...ICON_SVG_ATTRS}
>
	<path d={ATTACHMENT_ICON_PATHS[iconId]} />
</svg>
