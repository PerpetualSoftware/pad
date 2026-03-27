<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';

	// Dashboard is now the workspace home page — redirect there
	let wsSlug = $derived(page.params.workspace ?? '');

	$effect(() => {
		if (wsSlug) goto(`/${wsSlug}`, { replaceState: true });
	});
</script>
