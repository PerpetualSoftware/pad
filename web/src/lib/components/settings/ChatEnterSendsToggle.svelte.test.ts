import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { kb } from '$lib/chat/uiKeyboard.fixture.svelte';
// A real soft keyboard cannot be raised in jsdom: keyboardVisible is driven by hand.
vi.mock('$lib/stores/ui.svelte', async (orig) => {
	const actual = await orig<typeof import('$lib/stores/ui.svelte')>();
	return {
		...actual,
		uiStore: new Proxy(actual.uiStore, {
			get: (t, k) => (k === 'keyboardVisible' ? kb.visible : Reflect.get(t, k))
		})
	};
});
import ChatEnterSendsToggle from './ChatEnterSendsToggle.svelte';
import { inputMode } from '$lib/chat/inputMode.svelte';

const label = 'Chat: Enter sends (for a hardware keyboard)';

function setPointer(fine: boolean) {
	Object.defineProperty(window, 'matchMedia', {
		configurable: true,
		value: (q: string) => ({ matches: fine, media: q, addEventListener() {}, removeEventListener() {} })
	});
}

beforeEach(() => {
	localStorage.clear();
	inputMode.reset();
	setPointer(false);
});

afterEach(() => {
	cleanup();
	kb.visible = false;
	Reflect.deleteProperty(window, 'matchMedia');
});

describe('ChatEnterSendsToggle', () => {
	it('is offered on a coarse pointer with a labelled description', () => {
		const { getByLabelText, getByText } = render(ChatEnterSendsToggle);
		const toggle = getByLabelText(label);
		expect(toggle).not.toBeChecked();
		const description = getByText('For a tablet or phone with a keyboard attached. While the on-screen keyboard is up, Enter adds a line. Saved on this device; turning it off goes back to the default.');
		expect(toggle).toHaveAttribute('aria-describedby', description.id);
	});

	it('is not rendered on a fine pointer with nothing stored', () => {
		setPointer(true);
		const { queryByLabelText } = render(ChatEnterSendsToggle);
		expect(queryByLabelText(label)).toBeNull();
	});

	it('turning it on saves the choice per device, which survives a module reload', async () => {
		const { getByLabelText } = render(ChatEnterSendsToggle);
		await fireEvent.click(getByLabelText(label));
		expect(getByLabelText(label)).toBeChecked();
		expect(localStorage.getItem('pad-chat-enter-sends')).toBe('on');
		cleanup();
		vi.resetModules();
		const { inputMode: reloaded } = await import('$lib/chat/inputMode.svelte');
		expect(reloaded.hardware).toBe(true);
		expect(reloaded.stored).toBe(true);
	});

	it('turning it off removes the key and restores touch mode', async () => {
		inputMode.setHardware(true);
		const { getByLabelText } = render(ChatEnterSendsToggle);
		expect(getByLabelText(label)).toBeChecked();
		await fireEvent.click(getByLabelText(label));
		expect(getByLabelText(label)).not.toBeChecked();
		expect(localStorage.getItem('pad-chat-enter-sends')).toBeNull();
		expect(inputMode.hardware).toBe(false);
	});

	it('shows the saved choice as checked after a soft keyboard drops the tab to touch', () => {
		setPointer(true);
		inputMode.setHardware(true);
		const { getByLabelText } = render(ChatEnterSendsToggle);
		kb.visible = true;
		inputMode.softKeyboardUp();
		flushSync();
		expect(inputMode.hardware).toBe(false);
		expect(getByLabelText(label)).toBeChecked();
	});
});
