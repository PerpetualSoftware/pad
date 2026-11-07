<!-- Stand-in for BoardView in the collection page's draft-guard tests
     (TASK-4315). It owns only what the page binds: `draftText`. The test types
     a "draft" by calling the hook registered on globalThis, which writes through
     the binding exactly as a lane's inline composer does. -->
<script lang="ts">
	let { draftText = $bindable({}), ...rest }: { draftText?: Record<string, string>; [k: string]: unknown } = $props();
	const g = globalThis as { __draftBoard?: { setDraft: (v: Record<string, string>) => void } };
	g.__draftBoard = {
		setDraft(v) {
			draftText = v;
		}
	};
	void rest;
</script>
