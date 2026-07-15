<script lang="ts">
	import { page } from '$app/state';
	import ItemDetail from '$lib/components/items/ItemDetail.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';

	// Thin route wrapper (PLAN-2105 / TASK-2106). The item body now lives in
	// the shared, embeddable <ItemDetail> component. This wrapper renders it
	// full-page (embedded defaults false), so behavior is byte-identical to
	// before the extraction, and owns the SvelteKit route-module concern that
	// cannot live in a child component:
	//
	//   `export const snapshot` — scroll-position restoration wired through
	//   the route's snapshot API (BUG-1425). ItemDetail reports readiness (its
	//   loaded item matching the URL ref) via onReady, so the parked scroll
	//   offset un-parks only once the right item has rendered. The persistKey
	//   still keys on the item route's pathname, exactly as before.
	//
	// Route-away callbacks (onClose/onGone/onNavigateAway) are intentionally
	// NOT passed here — ItemDetail's embedded=false defaults reproduce the
	// original in-component `goto`s. The pane surface wires them (TASK-2107).

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let collSlug = $derived(page.params.collection ?? '');
	let ref = $derived(page.params.slug ?? '');

	// Threaded up from ItemDetail: true once its loaded item matches the URL
	// ref (see ItemDetail's scrollReady). Parks the snapshot restore until the
	// child's content is the URL's content — matching the pre-extraction
	// `ready()` predicate that read item/loading/itemSlug directly.
	let scrollReady = $state(false);

	const scrollRestoration = createScrollRestoration({
		ready: () => scrollReady,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;
</script>

<ItemDetail
	{username}
	{wsSlug}
	{collSlug}
	{ref}
	onReady={(ready) => (scrollReady = ready)}
/>
