<script lang="ts">
	// The rule under a new-password field, and a refusal for the password on
	// the field itself (TASK-2260). The input points at both through
	// aria-describedby={describedBy(id, error)}, so a screen reader hears the
	// rule with the field and the error when it appears.
	import { PASSWORD_RULE } from '$lib/auth/passwordRule';

	let { id, error = '' }: { id: string; error?: string } = $props();
</script>

<p class="password-rule" id="{id}-rule">{PASSWORD_RULE}</p>
{#if error}
	<p class="password-error" id="{id}-error" role="alert">{error}</p>
{/if}

<style>
	.password-rule {
		margin: calc(-1 * var(--space-2)) 0 0;
		font-size: 0.8rem;
		line-height: 1.4;
		color: var(--text-muted);
		text-align: left;
	}
	.password-error {
		margin: 0;
		font-size: 0.85rem;
		line-height: 1.4;
		color: var(--accent-red);
		text-align: left;
	}
</style>
