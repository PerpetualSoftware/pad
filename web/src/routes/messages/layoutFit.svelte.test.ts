// TASK-4315 PR 4: the standalone phone thread fits the visual viewport while the
// keyboard is up, so its composer is never covered (the page owns the box, not the Composer).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';
import { kb } from '$lib/chat/uiKeyboard.fixture.svelte';

const vp = vi.hoisted(() => ({ mobile: true, route: '/messages/[n]' }));
vi.mock('$app/state', () => ({
	page: {
		url: new URL('http://localhost/messages/3'),
		get route() {
			return { id: vp.route };
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return vp.mobile;
		}
	}
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		get keyboardVisible() {
			return kb.visible;
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: async () => ({ authenticated: true }),
		onIdentityChange: () => () => {},
		authenticated: true
	}
}));
vi.mock('$lib/stores/chatSetting.svelte', () => ({ chatSetting: { enabled: true } }));

import Layout from './+layout.svelte';

const child = createRawSnippet(() => ({ render: () => `<div>child</div>` }));
const settle = async () => {
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
		flushSync();
	}
};
const main = () => document.querySelector<HTMLElement>('.messages-main')!;

beforeEach(() => {
	kb.visible = false;
	vp.route = '/messages/[n]';
});
afterEach(() => {
	cleanup();
	kb.visible = false;
	Reflect.deleteProperty(window, 'visualViewport');
});

describe('standalone thread and the keyboard', () => {
	it('keeps the CSS height with no keyboard, and fits the visual viewport box while it is up', async () => {
		const vv = Object.assign(new EventTarget(), { height: 800, offsetTop: 0 });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		render(Layout, { props: { children: child } });
		await settle();
		expect(main().style.height).toBe('');
		vv.height = 430;
		vv.offsetTop = 12;
		kb.visible = true;
		await settle();
		expect(main().style.position).toBe('fixed');
		expect(main().style.top).toBe('12px');
		expect(main().style.height).toBe('430px');
		kb.visible = false;
		await settle();
		expect(main().style.height).toBe('');
		expect(main().style.position).toBe('');
	});

	it('the inbox (not a thread) is not fitted', async () => {
		vp.route = '/messages';
		const vv = Object.assign(new EventTarget(), { height: 430, offsetTop: 0 });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		kb.visible = true;
		render(Layout, { props: { children: child } });
		await settle();
		expect(main().style.height).toBe('');
	});
});
