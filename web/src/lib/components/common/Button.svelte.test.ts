// TASK-4515: Button's two additive props. Defaults must not move any existing caller.
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { createRawSnippet, flushSync } from 'svelte';
import Button from './Button.svelte';
import ButtonBind from './fixtures/ButtonBind.fixture.svelte';

const text = (s: string) => createRawSnippet(() => ({ render: () => `<span>${s}</span>` }));

afterEach(cleanup);

describe('Button', () => {
	it('renders the same classes and type as before by default', () => {
		const { container } = render(Button, { props: { children: text('Go') } });
		const b = container.querySelector('button')!;
		expect(b.className.split(/\s+/).filter((c) => !c.startsWith('svelte-')).sort()).toEqual(['btn', 'md', 'secondary']);
		expect(b.getAttribute('type')).toBe('button');
		expect(b.classList.contains('touch')).toBe(false);
	});

	it('touch adds the touch class', () => {
		const { container } = render(Button, { props: { touch: true, variant: 'primary', children: text('Send') } });
		const b = container.querySelector('button')!;
		expect(b.classList.contains('touch')).toBe(true);
		expect(b.classList.contains('primary')).toBe(true);
	});

	it('binds the rendered element', () => {
		let got: HTMLElement | undefined;
		const { container } = render(ButtonBind, { props: { onbound: (b) => (got = b) } });
		flushSync();
		expect(got).toBe(container.querySelector('button.bound'));
	});
});
