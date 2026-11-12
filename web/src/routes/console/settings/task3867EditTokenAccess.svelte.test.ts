import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';

const state = vi.hoisted(() => ({
 factor: 'totp', scopes: '["*"]', epoch: 0, verified: false,
 holdVerify: null as null | { release: () => void }, refuseRetry: false,
}));
vi.mock('$lib/api/client', () => {
 class PadApiError extends Error {
  code: string;
  constructor(error: { code: string; message: string }) { super(error.message); this.code = error.code; }
 }
 return { PadApiError, isPlanLimitError: () => false, api: {
  calendarFeeds: { list: vi.fn(async () => []), create: vi.fn(), revoke: vi.fn() },
  workspaces: { list: vi.fn(async () => []) },
  auth: {
   me: vi.fn(async () => ({ id: 'u1', name: 'Pat', email: 'pat@example.com', username: 'pat', password_set: true })),
   totp: { disable: vi.fn(), setup: vi.fn(), verify: vi.fn() },
   tokens: {
    list: vi.fn(async () => [{ id: 't1', name: 'ci-runner', scopes: state.scopes, prefix: 'pad_ab', created_at: '2026-10-01T00:00:00Z' }]),
    update: vi.fn(async (id: string, input: { name?: string; scopes?: string }) => {
     if (input.scopes === '["*"]' && (!state.verified || state.refuseRetry)) throw new PadApiError({ code: 'step_up_required', message: 'Verify your identity' });
     return { id, name: input.name ?? 'ci-runner', scopes: input.scopes ?? state.scopes, prefix: 'pad_ab', created_at: '2026-10-01T00:00:00Z' };
    }),
    create: vi.fn(), rotate: vi.fn(), delete: vi.fn(),
   },
   stepUp: {
    status: vi.fn(async () => ({ factor: state.factor, active_until: null })),
    sendEmailCode: vi.fn(async () => ({ sent: true, expires_at: '2026-11-12T12:10:00Z' })),
    verify: vi.fn(async () => {
     if (state.holdVerify) await new Promise<void>((r) => { state.holdVerify!.release = r; });
     state.verified = true; return { active_until: '2026-11-12T12:10:00Z' };
    }),
   },
   updateProfile: vi.fn(), unlinkProvider: vi.fn(), deleteAccount: vi.fn(), logout: vi.fn(), forgotPassword: vi.fn(),
  },
 } };
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: {
 cloudMode: false, emailConfigured: true, user: { id: 'u1', name: 'Pat', email: 'pat@example.com' }, userId: 'u1',
 get identityEpoch() { return state.epoch; },
 identityFence() { const epoch = state.epoch; return () => state.epoch === epoch; },
 onIdentityChange() { return () => {}; }, load: vi.fn(), ensureLoaded: vi.fn(),
} }));
const { api } = await import('$lib/api/client');
const { default: AccountSettings } = await import('./+page.svelte');
beforeEach(() => {
 vi.clearAllMocks();
 Object.assign(state, { factor: 'totp', scopes: '["*"]', epoch: 0, verified: false, holdVerify: null, refuseRetry: false });
});
async function openEdit() {
 render(AccountSettings);
 await waitFor(() => expect(screen.getByText('ci-runner')).toBeInTheDocument());
 await fireEvent.click(screen.getByRole('button', { name: 'Edit access' }));
 return screen.getByRole('dialog', { name: 'Edit API key access' });
}
async function widen(factor: string) {
 state.factor = factor; state.scopes = '["read"]';
 const dialog = await openEdit();
 await fireEvent.click(within(dialog).getByRole('radio', { name: 'Read and write' }));
 await fireEvent.click(within(dialog).getByRole('button', { name: 'Save changes' }));
 await waitFor(() => expect(api.auth.stepUp.status).toHaveBeenCalledOnce());
 return dialog;
}
async function verify(dialog: HTMLElement, label: string, value: string) {
 await fireEvent.input(await within(dialog).findByLabelText(label), { target: { value } });
 await fireEvent.click(within(dialog).getByRole('button', { name: 'Verify' }));
}

describe('TASK-3867: editing API key access', () => {
 it('narrows and updates the list without a prompt', async () => {
  const dialog = await openEdit();
  await fireEvent.click(within(dialog).getByRole('radio', { name: 'Read only' }));
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledWith('t1', { name: 'ci-runner', scopes: '["read"]' }));
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Edit API key access' })).toBeNull());
  expect(api.auth.stepUp.status).not.toHaveBeenCalled();
  expect(screen.getByTestId('token-access')).toHaveTextContent('Read only');
 });
 it('sends a name-only edit without rewriting scopes', async () => {
  const dialog = await openEdit();
  await fireEvent.input(within(dialog).getByLabelText('Key name'), { target: { value: 'Renamed' } });
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledWith('t1', { name: 'Renamed' }));
  expect(api.auth.stepUp.status).not.toHaveBeenCalled();
 });
 it('verifies TOTP and retries the same edit once', async () => {
  const dialog = await widen('totp'); await verify(dialog, 'Authenticator code', '123456');
  await waitFor(() => expect(api.auth.stepUp.verify).toHaveBeenCalledWith({ code: '123456' }));
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledTimes(2));
  expect(vi.mocked(api.auth.tokens.update).mock.calls[0]).toEqual(vi.mocked(api.auth.tokens.update).mock.calls[1]);
 });
 it('accepts a recovery code', async () => {
  const dialog = await widen('totp');
  await fireEvent.click(await within(dialog).findByRole('button', { name: 'Use a recovery code' }));
  await verify(dialog, 'Recovery code', 'ABCD-EFGH-IJKL-MNOP');
  await waitFor(() => expect(api.auth.stepUp.verify).toHaveBeenCalledWith({ recovery_code: 'ABCD-EFGH-IJKL-MNOP' }));
 });
 it('sends and verifies the emailed code', async () => {
  const dialog = await widen('email');
  await fireEvent.click(await within(dialog).findByRole('button', { name: 'Send code' }));
  await waitFor(() => expect(api.auth.stepUp.sendEmailCode).toHaveBeenCalledOnce());
  await verify(dialog, 'Email code', '654321');
  await waitFor(() => expect(api.auth.stepUp.verify).toHaveBeenCalledWith({ code: '654321' }));
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledTimes(2));
 });
 it('asks for a freshly entered password', async () => {
  const dialog = await widen('password');
  expect(await within(dialog).findByLabelText('Password')).toHaveValue('');
  await verify(dialog, 'Password', 'fresh-password');
  await waitFor(() => expect(api.auth.stepUp.verify).toHaveBeenCalledWith({ password: 'fresh-password' }));
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledTimes(2));
 });
 it('shows guidance when verification is unavailable', async () => {
  const dialog = await widen('unavailable');
  expect(await within(dialog).findByText(/create a new key and delete this one/)).toBeInTheDocument();
  expect(within(dialog).queryByRole('button', { name: 'Verify' })).toBeNull();
  expect(api.auth.stepUp.verify).not.toHaveBeenCalled();
  expect(api.auth.tokens.update).toHaveBeenCalledOnce();
 });
 it('reports a failed retry without looping', async () => {
  state.refuseRetry = true; const dialog = await widen('password'); await verify(dialog, 'Password', 'fresh-password');
  await waitFor(() => expect(api.auth.tokens.update).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(within(dialog).getByRole('alert')).toHaveTextContent('Verify your identity'));
  expect(api.auth.stepUp.status).toHaveBeenCalledOnce();
 });
 it('does not retry after an identity change during verification', async () => {
  state.holdVerify = { release: () => {} }; const dialog = await widen('password'); await verify(dialog, 'Password', 'fresh-password');
  await waitFor(() => expect(api.auth.stepUp.verify).toHaveBeenCalledOnce());
  state.epoch++; state.holdVerify.release(); await new Promise((r) => setTimeout(r, 0));
  expect(api.auth.tokens.update).toHaveBeenCalledOnce();
 });
});
