<script lang="ts">
	// The "▶ slug" and "N args" chips that say a playbook is invokable and what
	// it takes (PLAN-1377). The library cards had them since TASK-1399; the
	// workspace's own Playbooks list did not, so telling which of your own
	// playbooks you can call meant opening each one (TASK-2256, audit C65).
	// One component so the two pages say the same thing.
	import Chip from '$lib/components/common/Chip.svelte';

	interface Props {
		slug?: string | null;
		argCount?: number;
	}

	let { slug = null, argCount = 0 }: Props = $props();
</script>

{#if slug}
	<Chip
		size="sm"
		color="var(--accent-green)"
		title={`Run it by saying "run the ${slug} playbook" — or the shortcut for your agent: /pad ${slug} (Claude Code), $pad ${slug} (Codex), pad_playbook action=run ref=${slug} (MCP)`}
	><span class="sr-only">{'Invocation: '}</span><span class="slug-text">▶ {slug}</span></Chip>
{/if}
{#if argCount > 0}
	<Chip size="sm" color="var(--accent-amber)" title="Accepts {argCount} argument{argCount === 1 ? '' : 's'}">{argCount} arg{argCount === 1 ? '' : 's'}</Chip>
{/if}

<style>
	.slug-text { font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace); }
</style>
