// The owner of "where may a fixed Chat surface sit right now" (TASK-4315 PR 4).
// Call useViewportFit() during component init: its `box` is the visual viewport's
// {top, height} only while the soft keyboard is up (uiStore.keyboardVisible, touch
// only) and visualViewport exists, else null. Listeners and the pending frame are
// held only while the box is wanted.
import { uiStore } from '$lib/stores/ui.svelte';
import { watchFitBox, type FitBox } from './viewport';

export function useViewportFit(): { readonly box: FitBox | null } {
	let box = $state.raw<FitBox | null>(null);
	$effect(() => {
		if (!uiStore.keyboardVisible) return;
		const stop = watchFitBox((b) => (box = b));
		return () => {
			stop();
			box = null;
		};
	});
	return {
		get box() {
			return box;
		}
	};
}
