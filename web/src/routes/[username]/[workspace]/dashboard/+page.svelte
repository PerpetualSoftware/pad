<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';

	// Dashboard is now the workspace home page — redirect there
	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	$effect(() => {
		if (wsSlug) goto(`/${username}/${wsSlug}`, { replaceState: true });
	});
</script>
