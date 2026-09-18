/**
 * BUG-3105 — driven legs for `ShareDialog`, the least-guarded file in the
 * ItemDetail child population.
 *
 * Before this unit the file had NO fence of any kind: no epoch, no generation
 * counter, not even a captured slug. Every handler read the live props at
 * request time and committed whatever came back. It contributed ZERO paths to
 * BUG-3095's list, because that guard modelled only requests and every request
 * here is dispatched synchronously from a click — the damage is all on the
 * commit side.
 *
 * Two legs are driven here rather than left to the source guard:
 *
 *   - `handleRevoke`, the most destructive path: it DELETEs a grant and then
 *     commits the shortened grant list. The source guard proves a predicate is
 *     CALLED between the await and the commit; only a driven leg proves the
 *     branch actually returns and the commit does not happen.
 *   - `loadShareLinks`, the highest-stakes commit: a share link carries a
 *     bearer token, and the dialog renders a freshly minted one under "copy it
 *     now, it is only shown once". Painting that into whoever happens to be
 *     signed in when the response lands is the worst outcome in this file.
 */
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
      deleteShareLink: vi.fn(),
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

describe('ShareDialog identity fence (BUG-3105)', () => {
  it('PRECONDITION: the dialog issues its loads on open, so the legs below drive a real path', async () => {
    render();
    await settle();
    expect(apiMock.listItemGrants).toHaveBeenCalled();
    expect(apiMock.listItemShareLinks).toHaveBeenCalled();
  });

  it('CONTROL: with the identity unchanged, a loaded grant list IS committed', async () => {
    const d = deferred<unknown[]>();
    apiMock.listItemGrants.mockReturnValue(d.promise);
    render();
    await settle();

    d.resolve([{ id: 'g1', user_id: 'u9', user_email: 'someone@example.com', permission: 'view' }]);
    await settle();

    // The committed grant reaches the DOM — the positive half that makes the
    // absence assertion in the next test mean something.
    expect(target.textContent).toContain('someone@example.com');
  });

  it('an identity change while the grant list is in flight refuses the commit', async () => {
    const d = deferred<unknown[]>();
    apiMock.listItemGrants.mockReturnValue(d.promise);
    render();
    await settle();

    auth.moveIdentity();
    d.resolve([{ id: 'g1', user_id: 'u9', user_email: 'someone@example.com', permission: 'view' }]);
    await settle();

    expect(target.textContent).not.toContain('someone@example.com');
  });

  it('an identity change while SHARE LINKS are in flight refuses the commit (bearer token)', async () => {
    const d = deferred<unknown[]>();
    apiMock.listItemShareLinks.mockReturnValue(d.promise);
    render();
    await settle();

    auth.moveIdentity();
    d.resolve([{ id: 'sl1', token: 'SECRET-TOKEN-VALUE', url: 'http://x/share/SECRET-TOKEN-VALUE' }]);
    await settle();

    // The token must not reach the DOM of a dialog the next identity is reading.
    expect(target.textContent).not.toContain('SECRET-TOKEN-VALUE');
    expect(target.innerHTML).not.toContain('SECRET-TOKEN-VALUE');
  });

  it('CONTROL for the destructive path: an unchanged identity commits the revoke', async () => {
    apiMock.listItemGrants.mockResolvedValue([
      { id: 'g1', user_id: 'u9', user_email: 'revokeme@example.com', permission: 'view' },
    ]);
    const d = deferred<void>();
    apiMock.deleteItemGrant.mockReturnValue(d.promise);
    render();
    await settle();
    expect(target.textContent).toContain('revokeme@example.com');

    const revoke = target.querySelector<HTMLButtonElement>('.revoke-btn');
    expect(revoke, 'a revoke control must be reachable for this leg to mean anything').toBeTruthy();
    revoke!.click();
    flushSync();

    d.resolve();
    await settle();
    expect(target.textContent).not.toContain('revokeme@example.com');
  });

  it('an identity change during a REVOKE refuses the commit', async () => {
    apiMock.listItemGrants.mockResolvedValue([
      { id: 'g1', user_id: 'u9', user_email: 'revokeme@example.com', permission: 'view' },
    ]);
    const d = deferred<void>();
    apiMock.deleteItemGrant.mockReturnValue(d.promise);
    render();
    await settle();

    const revoke = target.querySelector<HTMLButtonElement>('.revoke-btn');
    expect(revoke, 'precondition: the revoke control is reachable').toBeTruthy();
    revoke!.click();
    flushSync();

    // The DELETE is already out — it was issued before any await in the handler,
    // and this fence cannot unsend it. What it refuses is the COMMIT: the next
    // identity must not be shown a grant list edited by the previous one's
    // action.
    auth.moveIdentity();
    d.resolve();
    await settle();

    expect(apiMock.deleteItemGrant).toHaveBeenCalled();
    expect(target.textContent).toContain('revokeme@example.com');
  });
});
