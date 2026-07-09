// Setup for the jsdom vitest project (see vitest.config.ts). Only loaded when
// the browser test deps are installed, so importing them here is safe.
import '@testing-library/jest-dom/vitest';

// jsdom does not implement the native <dialog> top-layer methods
// (`HTMLDialogElement.prototype.showModal` / `.close` are either missing or
// throw "Not implemented"). Modal.svelte drives the element through those, so
// polyfill a minimal, spec-shaped version that just reflects the `open`
// attribute + fires the `close` event. This exercises the component's
// open/close logic without a real UA.
if (typeof HTMLDialogElement !== 'undefined') {
	HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
		this.open = true;
		this.setAttribute('open', '');
	};
	HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement, returnValue?: string) {
		if (returnValue !== undefined) {
			this.returnValue = returnValue;
		}
		this.open = false;
		this.removeAttribute('open');
		this.dispatchEvent(new Event('close'));
	};
}
