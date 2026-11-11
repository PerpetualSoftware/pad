// @vitest-environment jsdom
import { describe, it, expect, beforeEach } from 'vitest';
import { isIOS, stopIOSFocusZoom } from './iosFocusZoom';

// BUG-4482: iOS zooms into any focused field whose text is under 16px, which
// pans the page sideways and drops the editor toolbar under the keyboard.

const IPHONE =
	'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148';
const ANDROID =
	'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Mobile Safari/537.36';
const MAC_SAFARI =
	'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15';
const BASE = 'width=device-width, initial-scale=1, viewport-fit=cover';

function nav(userAgent: string, platform = '', maxTouchPoints = 0) {
	return { userAgent, platform, maxTouchPoints };
}

function viewportMeta(): HTMLMetaElement {
	return document.querySelector('meta[name="viewport"]') as HTMLMetaElement;
}

describe('isIOS', () => {
	it('is true for an iPhone', () => {
		expect(isIOS(nav(IPHONE, 'iPhone', 5))).toBe(true);
	});
	it('is true for an iPad that reports itself as a Mac', () => {
		expect(isIOS(nav(MAC_SAFARI, 'MacIntel', 5))).toBe(true);
	});
	it('is false for a Mac without touch', () => {
		expect(isIOS(nav(MAC_SAFARI, 'MacIntel', 0))).toBe(false);
	});
	it('is false for Android', () => {
		expect(isIOS(nav(ANDROID, 'Linux armv81', 5))).toBe(false);
	});
});

describe('stopIOSFocusZoom', () => {
	beforeEach(() => {
		document.head.innerHTML = `<meta name="viewport" content="${BASE}">`;
	});

	it('adds maximum-scale=1 on iOS', () => {
		stopIOSFocusZoom(document, nav(IPHONE, 'iPhone', 5));
		expect(viewportMeta().content).toBe(`${BASE}, maximum-scale=1`);
	});

	it('leaves Android alone, where maximum-scale would block pinch zoom', () => {
		stopIOSFocusZoom(document, nav(ANDROID, 'Linux armv81', 5));
		expect(viewportMeta().content).toBe(BASE);
	});

	it('adds it once when called again', () => {
		stopIOSFocusZoom(document, nav(IPHONE, 'iPhone', 5));
		stopIOSFocusZoom(document, nav(IPHONE, 'iPhone', 5));
		expect(viewportMeta().content).toBe(`${BASE}, maximum-scale=1`);
	});

	it('does nothing without a viewport meta', () => {
		document.head.innerHTML = '';
		expect(() => stopIOSFocusZoom(document, nav(IPHONE, 'iPhone', 5))).not.toThrow();
	});
});
