<script lang="ts">
	// Click-to-load tutorial player (TASK-3452), the in-app twin of
	// getpad.dev/learn's facade (pad-web TASK-3450). Until the viewer presses
	// play this is a same-origin poster and a button: nothing is requested from
	// YouTube or Google. On play it mounts a youtube-nocookie iframe in the same
	// 16:9 box. Only Pad Cloud renders it; its page CSP allows exactly that
	// frame (spaHandler), and a self-hosted server's does not.
	//
	// Chapters seek through the player's postMessage interface (enablejsapi=1),
	// so YouTube's API script is never loaded.
	let { videoId, title, poster }: { videoId: string | null; title: string; poster: string | null } = $props();

	let start = $state(0);
	let loaded = $state(false);
	let iframe = $state<HTMLIFrameElement | null>(null);

	const ORIGIN = 'https://www.youtube-nocookie.com';
	const src = $derived(
		videoId
			? `${ORIGIN}/embed/${encodeURIComponent(videoId)}?autoplay=1&rel=0&enablejsapi=1&start=${Math.floor(start)}` +
					(typeof location !== 'undefined' ? `&origin=${encodeURIComponent(location.origin)}` : '')
			: ''
	);

	function command(func: string, args: unknown[] = []) {
		iframe?.contentWindow?.postMessage(JSON.stringify({ event: 'command', func, args }), ORIGIN);
	}

	/** Jump to `t` seconds, loading the player first if it isn't yet. */
	export function seek(t: number) {
		if (!videoId) return;
		if (!loaded) {
			start = t;
			loaded = true;
			return;
		}
		command('seekTo', [t, true]);
		command('playVideo');
	}

	$effect(() => {
		if (loaded && iframe) iframe.focus();
	});
</script>

<div class="player">
	{#if loaded && videoId}
		<iframe
			bind:this={iframe}
			{src}
			title={`${title} (video)`}
			allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture; web-share"
			referrerpolicy="strict-origin-when-cross-origin"
			allowfullscreen
		></iframe>
	{:else}
		{#if poster}
			<img src={poster} alt="" width="1280" height="720" decoding="async" />
		{/if}
		{#if videoId}
			<button type="button" class="play" onclick={() => seek(start)} aria-label={`Play video: ${title}`}>
				<span class="play-disc" aria-hidden="true">
					<svg viewBox="0 0 24 24" fill="currentColor"><path d="M8 5.14v13.72a1 1 0 0 0 1.52.85l10.93-6.86a1 1 0 0 0 0-1.7L9.52 4.29A1 1 0 0 0 8 5.14z" /></svg>
				</span>
			</button>
			<span class="from-yt" aria-hidden="true">Plays from YouTube</span>
		{:else}
			<span class="soon">Video coming soon</span>
		{/if}
	{/if}
</div>

<style>
	.player {
		position: relative;
		width: 100%;
		aspect-ratio: 16 / 9;
		overflow: hidden;
		border-radius: var(--radius);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
	}
	img,
	iframe,
	.play {
		position: absolute;
		inset: 0;
		width: 100%;
		height: 100%;
		border: 0;
	}
	img {
		object-fit: cover;
	}
	.play {
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgb(0 0 0 / 0.1);
		cursor: pointer;
	}
	.play:hover {
		background: rgb(0 0 0 / 0.25);
	}
	.play:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: -4px;
	}
	.play-disc {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 4rem;
		height: 4rem;
		border-radius: 999px;
		background: var(--accent-blue);
		color: #fff;
	}
	.play-disc svg {
		width: 1.75rem;
		height: 1.75rem;
		margin-left: 0.25rem;
	}
	.from-yt,
	.soon {
		position: absolute;
		bottom: var(--space-2);
		right: var(--space-3);
		padding: 0.1rem 0.5rem;
		border-radius: 4px;
		background: rgb(0 0 0 / 0.6);
		color: #fff;
		font-size: 0.75rem;
		pointer-events: none;
	}
	.soon {
		left: var(--space-3);
		right: auto;
		font-size: 0.85rem;
	}
</style>
