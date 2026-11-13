<script lang="ts" module>
	export type MobileToolbarIconName =
		| 'plus'
		| 'bold'
		| 'italic'
		| 'strikethrough'
		| 'heading-2'
		| 'heading-3'
		| 'list'
		| 'list-ordered'
		| 'list-todo'
		| 'outdent'
		| 'indent'
		| 'code'
		| 'quote'
		| 'paperclip';
</script>

<script lang="ts">
	// Stroke icons in the style the rest of the app draws inline (24px grid,
	// 2px round stroke). The button carries the accessible name, so the
	// drawing is hidden from assistive technology.
	let { name }: { name: MobileToolbarIconName } = $props();
</script>

<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
	{#if name === 'plus'}
		<path d="M5 12h14" /><path d="M12 5v14" />
	{:else if name === 'bold'}
		<path d="M14 12a4 4 0 0 0 0-8H6v8" /><path d="M15 20a4 4 0 0 0 0-8H6v8Z" />
	{:else if name === 'italic'}
		<line x1="19" x2="10" y1="4" y2="4" /><line x1="14" x2="5" y1="20" y2="20" /><line x1="15" x2="9" y1="4" y2="20" />
	{:else if name === 'strikethrough'}
		<path d="M16 4H9a3 3 0 0 0-2.83 4" /><path d="M14 12a4 4 0 0 1 0 8H6" /><line x1="4" x2="20" y1="12" y2="12" />
	{:else if name === 'heading-2'}
		<path d="M4 12h8" /><path d="M4 18V6" /><path d="M12 18V6" /><path d="M21 18h-4c0-4 4-3 4-6 0-1.5-2-2.5-4-1" />
	{:else if name === 'heading-3'}
		<path d="M4 12h8" /><path d="M4 18V6" /><path d="M12 18V6" /><path d="M17.5 10.5c1.7-1 3.5 0 3.5 1.5a2 2 0 0 1-2 2" /><path d="M17 17.5c2 1.5 4 .3 4-1.5a2 2 0 0 0-2-2" />
	{:else if name === 'list'}
		<line x1="8" x2="21" y1="6" y2="6" /><line x1="8" x2="21" y1="12" y2="12" /><line x1="8" x2="21" y1="18" y2="18" /><line x1="3" x2="3.01" y1="6" y2="6" /><line x1="3" x2="3.01" y1="12" y2="12" /><line x1="3" x2="3.01" y1="18" y2="18" />
	{:else if name === 'list-ordered'}
		<line x1="10" x2="21" y1="6" y2="6" /><line x1="10" x2="21" y1="12" y2="12" /><line x1="10" x2="21" y1="18" y2="18" /><path d="M4 6h1v4" /><path d="M4 10h2" /><path d="M6 18H4c0-1 2-2 2-3s-1-1.5-2-1" />
	{:else if name === 'list-todo'}
		<rect x="3" y="5" width="6" height="6" rx="1" /><path d="m3 17 2 2 4-4" /><path d="M13 6h8" /><path d="M13 12h8" /><path d="M13 18h8" />
	{:else if name === 'outdent'}
		<polyline points="7 8 3 12 7 16" /><line x1="21" x2="11" y1="12" y2="12" /><line x1="21" x2="11" y1="6" y2="6" /><line x1="21" x2="11" y1="18" y2="18" />
	{:else if name === 'indent'}
		<polyline points="3 8 7 12 3 16" /><line x1="21" x2="11" y1="12" y2="12" /><line x1="21" x2="11" y1="6" y2="6" /><line x1="21" x2="11" y1="18" y2="18" />
	{:else if name === 'code'}
		<polyline points="16 18 22 12 16 6" /><polyline points="8 6 2 12 8 18" />
	{:else if name === 'quote'}
		<path d="M17 6H3" /><path d="M21 12H8" /><path d="M21 18H8" /><path d="M3 12v6" />
	{:else if name === 'paperclip'}
		<path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48" />
	{/if}
</svg>
