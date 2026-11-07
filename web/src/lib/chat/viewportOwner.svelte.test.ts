// TASK-4315 PR 4: the visible-box owner. While the soft keyboard is up it reports the
// visual viewport's box ({top, height}) so a fixed surface can fit inside it; with the
// keyboard down, or without visualViewport, it reports null and holds nothing.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { kb } from './uiKeyboard.fixture.svelte';

vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		get keyboardVisible() {
			return kb.visible;
		}
	}
}));

import { useViewportFit } from './viewportOwner.svelte';

type VV = EventTarget & { height: number; offsetTop: number };
let rafQueue: Array<{ id: number; cb: FrameRequestCallback }> = [];
let rafId = 0;
let roots: Array<() => void> = [];

function fakeVV(height = 500, offsetTop = 0): VV {
	const vv = Object.assign(new EventTarget(), { height, offsetTop });
	Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
	return vv;
}
function mount() {
	let fit!: ReturnType<typeof useViewportFit>;
	const stop = $effect.root(() => {
		fit = useViewportFit();
	});
	roots.push(stop);
	flushSync();
	return { fit, stop };
}
const frame = () => {
	const q = rafQueue;
	rafQueue = [];
	for (const f of q) f.cb(0);
	flushSync();
};

beforeEach(() => {
	kb.visible = false;
	rafQueue = [];
	vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
		rafQueue.push({ id: ++rafId, cb });
		return rafId;
	});
	vi.stubGlobal('cancelAnimationFrame', (id: number) => {
		rafQueue = rafQueue.filter((f) => f.id !== id);
	});
});
afterEach(() => {
	for (const s of roots.splice(0)) s();
	Reflect.deleteProperty(window, 'visualViewport');
	vi.unstubAllGlobals();
});

describe('viewport owner', () => {
	it('is null and listens to nothing while the keyboard is down', () => {
		const vv = fakeVV();
		const add = vi.spyOn(vv, 'addEventListener');
		const { fit } = mount();
		expect(fit.box).toBeNull();
		expect(add).not.toHaveBeenCalled();
	});

	it('is null without visualViewport even with the keyboard up', () => {
		const { fit } = mount();
		kb.visible = true;
		flushSync();
		expect(fit.box).toBeNull();
	});

	it('reports the visual viewport box when the keyboard comes up', () => {
		fakeVV(420, 30);
		const { fit } = mount();
		kb.visible = true;
		flushSync();
		expect(fit.box).toEqual({ top: 30, height: 420 });
	});

	it('follows resize and scroll, coalesced to one frame', () => {
		const vv = fakeVV(420, 0);
		const { fit } = mount();
		kb.visible = true;
		flushSync();
		vv.height = 400;
		vv.dispatchEvent(new Event('resize'));
		vv.offsetTop = 25;
		vv.dispatchEvent(new Event('scroll'));
		vv.dispatchEvent(new Event('scroll'));
		expect(rafQueue).toHaveLength(1);
		expect(fit.box).toEqual({ top: 0, height: 420 });
		frame();
		expect(fit.box).toEqual({ top: 25, height: 400 });
	});

	it('re-measures on orientationchange', () => {
		const vv = fakeVV(420, 0);
		const { fit } = mount();
		kb.visible = true;
		flushSync();
		vv.height = 250;
		window.dispatchEvent(new Event('orientationchange'));
		frame();
		expect(fit.box).toEqual({ top: 0, height: 250 });
	});

	it('goes back to null and lets go of everything when the keyboard hides', () => {
		const vv = fakeVV(420, 0);
		const remove = vi.spyOn(vv, 'removeEventListener');
		const { fit } = mount();
		kb.visible = true;
		flushSync();
		vv.dispatchEvent(new Event('resize'));
		expect(rafQueue).toHaveLength(1);
		kb.visible = false;
		flushSync();
		expect(fit.box).toBeNull();
		expect(rafQueue).toHaveLength(0);
		expect(remove.mock.calls.map((c) => c[0]).sort()).toEqual(['resize', 'scroll']);
	});

	it('unmounting removes the listeners and cancels a pending frame', () => {
		const vv = fakeVV(420, 0);
		const add = vi.spyOn(vv, 'addEventListener');
		const remove = vi.spyOn(vv, 'removeEventListener');
		const { stop } = mount();
		kb.visible = true;
		flushSync();
		vv.dispatchEvent(new Event('resize'));
		stop();
		expect(rafQueue).toHaveLength(0);
		for (const [i, c] of add.mock.calls.entries()) expect(remove.mock.calls[i][1]).toBe(c[1]);
		expect(remove).toHaveBeenCalledTimes(2);
	});
});
