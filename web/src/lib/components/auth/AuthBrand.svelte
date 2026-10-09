<script lang="ts">
	// The Pad mark and wordmark for the auth pages (TASK-3509). The mark is the
	// app icon's 3x3 grid (static/pad-mark.svg, drawn from the mobile app icon);
	// the dark backing tile and glow are left out so it reads in both themes.
	let { size = 26 }: { size?: number } = $props();
</script>

<span class="auth-brand">
	<img class="auth-brand-mark" src="/pad-mark.svg" width={size} height={size} alt="" />
	<span class="auth-brand-word">pad</span>
</span>

<style>
	.auth-brand {
		display: inline-flex;
		align-items: center;
		gap: 10px;
		color: var(--text-primary);
	}
	.auth-brand-mark {
		display: block;
		flex-shrink: 0;
	}
	.auth-brand-word {
		font-size: 1.2rem;
		font-weight: 700;
		letter-spacing: -0.03em;
		line-height: 1;
	}
</style>
