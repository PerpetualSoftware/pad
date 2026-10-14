// A reactive counter for hoisted mocks (BUG-3699): `vi.hoisted` runs before
// the Svelte runtime, so a mock that a page reads inside `$derived` cannot
// declare `$state` itself. Binding its `{ read, write }` slots here makes a
// read a real dependency, so a write re-runs the page's derivations.
let count = $state(0);

export function bindReactiveCounter(hook: {
	read: null | (() => number);
	write: null | ((n: number) => void);
}): void {
	hook.read = () => count;
	hook.write = (n: number) => {
		count = n;
	};
}
