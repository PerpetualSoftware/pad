// TASK-2193: a revoke in the share dialog asks first, in the row, naming what
// goes. One click on the × used to revoke at once, and a share link's token
// is shown once, so a revoked link cannot be recreated identically.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

const { calls } = vi.hoisted(() => ({ calls: [] as string[] }));

const apiMock = vi.hoisted(() => ({
  listCollectionGrants: vi.fn(),
  listItemGrants: vi.fn(),
  deleteItemGrant: vi.fn(),
  deleteCollectionGrant: vi.fn(),
  listItemShareLinks: vi.fn(),
  listCollectionShareLinks: vi.fn(),
  deleteShareLink: vi.fn(),
}));

vi.mock('$lib/api/client', () => ({
  api: {
    grants: {
      listCollectionGrants: (...a: unknown[]) => apiMock.listCollectionGrants(...a),
      listItemGrants: (...a: unknown[]) => apiMock.listItemGrants(...a),
      deleteCollectionGrant: (...a: unknown[]) => apiMock.deleteCollectionGrant(...a),
      deleteItemGrant: (...a: unknown[]) => apiMock.deleteItemGrant(...a),
      createCollectionGrant: vi.fn(),
      createItemGrant: vi.fn(),
    },
    shareLinks: {
      listCollectionShareLinks: (...a: unknown[]) => apiMock.listCollectionShareLinks(...a),
      listItemShareLinks: (...a: unknown[]) => apiMock.listItemShareLinks(...a),
      createCollectionShareLink: vi.fn(),
      createItemShareLink: vi.fn(),
      deleteShareLink: (...a: unknown[]) => apiMock.deleteShareLink(...a),
    },
  },
}));

const auth = vi.hoisted(() => {
  let epoch = 0;
  return {
    get identityEpoch() { return epoch; },
    get userId() { return 'u1'; },
    get user() { return { id: 'u1', name: 'A' }; },
    get authenticated() { return true; },
    moveIdentity() { epoch += 1; },
    reset() { epoch = 0; },
    identityFence() {
      const captured = epoch;
      return () => epoch === captured;
    },
    // `toast.svelte.ts` subscribes at MODULE LOAD and the dialog imports it, so
    // a double without this throws before any test runs.
    onIdentityChange(_fn: (p: string) => void) { return () => {}; },
  };
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

import ShareDialog from './ShareDialog.svelte';

let target: HTMLElement;
let instance: ReturnType<typeof mount> | null = null;

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

async function settle() {
  await Promise.resolve();
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
  flushSync();
}

function render() {
  target = document.body.appendChild(document.createElement('div'));
  instance = mount(ShareDialog, {
    target,
    props: {
      wsSlug: 'ws',
      type: 'item',
      targetSlug: 'ITEM-1',
      targetName: 'Item One',
      open: true,
    } as Record<string, unknown>,
  });
  flushSync();
}

beforeEach(() => {
  calls.length = 0;
  auth.reset();
  for (const fn of Object.values(apiMock)) (fn as ReturnType<typeof vi.fn>).mockReset();
  apiMock.listItemGrants.mockResolvedValue([]);
  apiMock.listItemShareLinks.mockResolvedValue([]);
  apiMock.deleteItemGrant.mockResolvedValue(undefined);
});

afterEach(() => {
  if (instance) unmount(instance);
  instance = null;
  if (target) target.remove();
  vi.clearAllMocks();
});

describe('TASK-2193: revoking from the share dialog asks first', () => {
  it('the grant × asks, naming who loses access; Cancel revokes nothing', async () => {
    apiMock.listItemGrants.mockResolvedValue([
      { id: 'g1', user_id: 'u9', user_email: 'revokeme@example.com', user_name: 'Rev Okee', permission: 'view' },
    ]);
    render();
    await settle();
    target.querySelector<HTMLButtonElement>('.revoke-btn')!.click();
    flushSync();
    expect(apiMock.deleteItemGrant).not.toHaveBeenCalled();
    const confirm = target.querySelector('.revoke-confirm');
    expect(confirm?.textContent).toMatch(/Revoke .+ access\?/);
    target.querySelector<HTMLButtonElement>('.revoke-confirm-no')!.click();
    flushSync();
    expect(target.querySelector('.revoke-confirm')).toBeNull();
    expect(target.querySelector('.revoke-btn')).toBeTruthy();
    expect(apiMock.deleteItemGrant).not.toHaveBeenCalled();
  });

  it('the grant confirm revokes once', async () => {
    apiMock.listItemGrants.mockResolvedValue([
      { id: 'g1', user_id: 'u9', user_email: 'revokeme@example.com', permission: 'view' },
    ]);
    render();
    await settle();
    target.querySelector<HTMLButtonElement>('.revoke-btn')!.click();
    flushSync();
    target.querySelector<HTMLButtonElement>('.revoke-confirm-yes')!.click();
    await settle();
    expect(apiMock.deleteItemGrant).toHaveBeenCalledTimes(1);
    expect(target.textContent).not.toContain('revokeme@example.com');
  });

  it('the share-link × says the link cannot be recreated, and its confirm revokes it', async () => {
    apiMock.listItemShareLinks.mockResolvedValue([
      { id: 'l1', token: '', view_count: 0, created_at: '2026-10-01T00:00:00Z', require_auth: false },
    ]);
    apiMock.deleteShareLink.mockResolvedValue(undefined);
    render();
    await settle();
    const linkRevoke = target.querySelector<HTMLButtonElement>('.revoke-btn[title="Revoke share link"]');
    expect(linkRevoke, 'precondition: the link row renders its revoke control').toBeTruthy();
    linkRevoke!.click();
    flushSync();
    expect(apiMock.deleteShareLink).not.toHaveBeenCalled();
    expect(target.querySelector('.revoke-confirm')?.textContent).toMatch(/can.t be recreated/);
    target.querySelector<HTMLButtonElement>('.revoke-confirm-yes')!.click();
    await settle();
    expect(apiMock.deleteShareLink).toHaveBeenCalledTimes(1);
  });
});
