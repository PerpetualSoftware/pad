<script lang="ts">
	import { avatarInitial, avatarTone } from '$lib/chat/avatar';

	// A rounded-square initial tinted from the author key (TASK-4515). Decoration: the name is
	// always beside it as text. Text uses the Chip recipe so light-mode initials hold AA.
	let { key, size = 'sm' }: { key: string; size?: 'sm' | 'lg' } = $props();
</script>

<span class="avatar {size}" style:--tone="var({avatarTone(key)})" aria-hidden="true">{avatarInitial(key)}</span>

<style>
	.avatar {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		width: 20px;
		height: 20px;
		border-radius: 6px;
		font-size: 11px;
		font-weight: 700;
		line-height: 1;
		background: color-mix(in srgb, var(--tone) 22%, transparent);
		color: color-mix(in srgb, var(--tone) var(--chip-text-mix), #000);
	}
	.avatar.lg {
		width: 36px;
		height: 36px;
		border-radius: 10px;
		font-size: 15px;
	}
</style>
