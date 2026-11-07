// A reactive stand-in for chatSurface in bar tests: `cs.open` is isOpen, the spies record calls.
import { vi } from 'vitest';

export const cs = $state({ open: false });
export const parked: Array<() => void> = [];
export const chatSurfaceMock = {
	get isOpen() {
		return cs.open;
	},
	open: vi.fn(async (_href: string) => {}),
	close: vi.fn(),
	// Mirrors the real contract: false (nothing done) when Chat is closed, true once parked.
	closeThen: vi.fn((run: () => void) => {
		if (!cs.open) return false;
		parked.push(run);
		return true;
	})
};
