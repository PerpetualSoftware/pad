// TASK-4315 PR 2a: MessageBody renders sanitised markdown and adds, outside the
// sanitised HTML, Copy code buttons and the image-failure note.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import MessageBody from './MessageBody.svelte';

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

const UUID = '0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';

describe('MessageBody', () => {
	it('keeps the .body hook and renders markdown elements', () => {
		const { container } = render(MessageBody, { props: { body: '**hi** <b>x</b>' } });
		const b = container.querySelector('.body.md')!;
		expect(b.querySelector('strong')!.textContent).toBe('hi');
		expect(b.querySelector('b')).toBeNull();
	});

	it('adds one Copy code button per fenced block, outside the rendered markup, and copies the code', async () => {
		const writeText = vi.fn().mockResolvedValue(undefined);
		Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
		const { container } = render(MessageBody, { props: { body: '```\nfirst <b>\n```\n\n```\nsecond\n```' } });
		const buttons = container.querySelectorAll('button.md-copy');
		expect(buttons).toHaveLength(2);
		expect(buttons[0].previousElementSibling!.tagName).toBe('PRE');
		await fireEvent.click(buttons[0]);
		expect(writeText).toHaveBeenCalledWith('first <b>\n');
		await vi.waitFor(() => expect(buttons[0].textContent).toBe('Copied'));
	});

	it('a message cannot supply its own button', () => {
		const { container } = render(MessageBody, { props: { body: '<button class="md-copy">x</button>' } });
		expect(container.querySelectorAll('button')).toHaveLength(0);
	});

	it('re-renders without accumulating buttons when the body changes', async () => {
		const r = render(MessageBody, { props: { body: '```\na\n```' } });
		await r.rerender({ body: '```\nb\n```\n\n```\nc\n```' });
		await tick();
		expect(r.container.querySelectorAll('pre')).toHaveLength(2);
		expect(r.container.querySelectorAll('button.md-copy')).toHaveLength(2);
		await r.rerender({ body: 'no code' });
		await tick();
		expect(r.container.querySelectorAll('button.md-copy')).toHaveLength(0);
	});

	it('an attachment image that fails to load becomes "Attachment unavailable."', async () => {
		const src = `/api/v1/workspaces/docapp/attachments/${UUID}`;
		const { container } = render(MessageBody, { props: { body: `![shot](${src})` } });
		const img = container.querySelector('img')!;
		expect(img.getAttribute('loading')).toBe('lazy');
		await fireEvent.error(img);
		expect(container.querySelector('img')).toBeNull();
		expect(container.querySelector('.md-note')!.textContent).toBe('Attachment unavailable.');
	});

	it('a restricted ref keeps the .ref-restricted hook inside .body', () => {
		const refs = new Map([['docapp\u0000TASK-2', { workspace: 'docapp', ref: 'TASK-2', state: 'restricted' as const }]]);
		const { container } = render(MessageBody, { props: { body: '[[TASK-2]]', refs, ctxWorkspace: 'docapp' } });
		expect(container.querySelector('.body .ref-restricted')!.textContent).toBe('restricted item');
	});
});
