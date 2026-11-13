import type { Action } from 'svelte/action';

/** Which ends of a sideways-scrolling row still hide content. */
export type ScrollHint = { start: boolean; end: boolean };

/**
 * Reports, on mount, on every scroll and on every resize, whether the node
 * has hidden content before or after what it shows. The toolbar uses it to
 * fade the edge that has more buttons behind it. A 1px slack absorbs the
 * fractional scrollLeft some browsers report at the very end.
 */
export const scrollHint: Action<HTMLElement, (hint: ScrollHint) => void> = (node, onChange) => {
	let report = onChange;
	const measure = () => {
		const max = node.scrollWidth - node.clientWidth;
		report({ start: node.scrollLeft > 1, end: node.scrollLeft < max - 1 });
	};
	node.addEventListener('scroll', measure, { passive: true });
	const resize = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(measure);
	resize?.observe(node);
	measure();
	return {
		update(next) {
			report = next;
			measure();
		},
		destroy() {
			node.removeEventListener('scroll', measure);
			resize?.disconnect();
		},
	};
};
