// TASK-3619: a comment upload refused for the storage limit raises the
// plan-limit prompt, but only for the identity that started it (codex r1).
import { describe, it, expect, vi, beforeEach } from 'vitest';

const m = vi.hoisted(() => ({ epoch: 0, upload: vi.fn(), shown: vi.fn() }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityFence: () => {
			const at = m.epoch;
			return () => m.epoch === at;
		},
	},
}));
vi.mock('$lib/billing/planLimitToast', () => ({ showPlanLimitToast: m.shown }));
vi.mock('$lib/api/client', async (orig) => {
	const real = await orig<typeof import('$lib/api/client')>();
	return { ...real, api: { attachments: { upload: m.upload } } };
});

import { uploadIntoTextarea } from './commentAttachments';
import { PadApiError } from '$lib/api/client';

function run(): { onError: ReturnType<typeof vi.fn>; settled: Promise<void> } {
	let value = '';
	let pending = 0;
	let resolveSettled!: () => void;
	const settled = new Promise<void>((r) => (resolveSettled = r));
	const onError = vi.fn();
	// Node project: only the selection offsets are read.
	const ta = { selectionStart: 0, selectionEnd: 0 } as HTMLTextAreaElement;
	uploadIntoTextarea([new File(['x'], 'a.txt')], ta, 'ws', {
		getValue: () => value,
		setValue: (v: string) => (value = v),
		onPendingDelta: (d: number) => {
			pending += d;
			if (pending === 0) resolveSettled();
		},
		onError,
	});
	return { onError, settled };
}

const refusal = () => new PadApiError({ code: 'plan_limit_exceeded', message: 'over', details: { plan: 'pro', payer_is_you: true } });

beforeEach(() => {
	m.epoch = 0;
	m.upload.mockReset();
	m.shown.mockReset();
});

describe('comment upload past the storage limit', () => {
	it('same identity: the prompt, not the raw error', async () => {
		m.upload.mockRejectedValue(refusal());
		const { onError, settled } = run();
		await settled;
		expect(m.shown).toHaveBeenCalledTimes(1);
		expect(onError).not.toHaveBeenCalled();
	});

	it('identity changed while in flight: no prompt naming the previous owner', async () => {
		let reject!: (e: unknown) => void;
		m.upload.mockReturnValue(new Promise((_, r) => (reject = r)));
		const { onError, settled } = run();
		m.epoch += 1;
		reject(refusal());
		await settled;
		expect(m.shown).not.toHaveBeenCalled();
		expect(onError).not.toHaveBeenCalled();
	});
});
