import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { ComponentProps } from 'svelte';
import AgendaToolbar from './AgendaToolbar.svelte';
import type { Workspace } from '$lib/types';

const viewport = vi.hoisted(() => ({ isMobile: false }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport }));
const workspaces = Array.from({ length: 30 }, (_, i) => ({ id: `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`, name: `Workspace ${String(i).padStart(2, '0')}`, slug: `space-${i}`, owner_username: 'alice' } as Workspace));
const groups = [ { id: 'all', name: 'All', kind: 'all' as const, workspace_ids: [] }, { id: 'mine', name: 'Mine', kind: 'mine' as const, workspace_ids: [] }, { id: 'team', name: 'Team', kind: 'named' as const, workspace_ids: [] } ];
function props(extra = {}): ComponentProps<typeof AgendaToolbar> {
 return { audience: 'all', counts: { mine: 0, agents: 0, all: 0 }, onaudience: vi.fn(), mode: 'week', period: 'This week', zone: 'UTC', groups, selection: { kind: 'group', id: 'all' }, workspaces: [...workspaces].reverse(), workspaceLabel: (ws: Workspace) => ws.name, workspacesLoading: false, workspacesError: false, onretryworkspaces: vi.fn(), onretrygroups: vi.fn(), showCompleted: true, onmode: vi.fn(), onmove: vi.fn(), ontoday: vi.fn(), ongroup: vi.fn(), onworkspace: vi.fn(), onmanage: vi.fn(), onshowcompleted: vi.fn(), ...extra };
}
const chooser = () => screen.getByRole('menuitem', { name: /Choose a workspace/ });
const trigger = () => screen.getByRole('button', { name: /^Workspaces:/ });
const back = () => screen.getByRole('menuitem', { name: 'Back' });
beforeEach(() => { viewport.isMobile = false; });
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('agenda workspace chooser', () => {
 it('keeps 30 workspaces out of the root, sorts the full list, and selects by ID in two activations', async () => {
  const p = props(); render(AgendaToolbar, { props: p }); await fireEvent.click(trigger());
  expect(screen.getAllByRole('menuitemradio').map(el => el.querySelector('.mi-label')?.textContent)).toEqual(['All workspaces', 'My workspaces', 'Team']);
  expect(screen.getByRole('menuitem', { name: 'Manage groups…' })).toBeTruthy();
  expect(screen.queryByText('Workspace 00')).toBeNull(); await fireEvent.click(chooser());
  const rows = screen.getAllByRole('menuitemradio'); expect(rows).toHaveLength(30);
  expect(rows[0]).toHaveTextContent('Workspace 00'); expect(rows[29]).toHaveTextContent('Workspace 29');
  expect(screen.queryByRole('textbox')).toBeNull(); expect(p.onworkspace).not.toHaveBeenCalled();
  await fireEvent.click(rows[29]); expect(p.onworkspace).toHaveBeenCalledWith(workspaces[29].id);
  expect(screen.queryByRole('menu')).toBeNull(); expect(trigger()).toHaveFocus();
  await fireEvent.click(trigger()); expect(chooser()).toBeTruthy();
 });
 it('follows desktop focus through Back, wrapping arrows, Home/End and Escape', async () => {
  render(AgendaToolbar, { props: props() }); await fireEvent.click(trigger());
  await waitFor(() => expect(screen.getAllByRole('menuitemradio')[0]).toHaveFocus());
  await fireEvent.click(chooser()); await waitFor(() => expect(back()).toHaveFocus());
  await fireEvent.keyDown(back(), { key: 'ArrowUp' }); expect(screen.getAllByRole('menuitemradio').at(-1)).toHaveFocus();
  await fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' }); expect(back()).toHaveFocus();
  await fireEvent.keyDown(back(), { key: 'End' }); expect(screen.getAllByRole('menuitemradio').at(-1)).toHaveFocus();
  await fireEvent.keyDown(document.activeElement!, { key: 'Home' }); expect(back()).toHaveFocus();
  await fireEvent.click(back()); await waitFor(() => expect(screen.getAllByRole('menuitemradio')[0]).toHaveFocus());
  await fireEvent.click(chooser()); await fireEvent.keyDown(back(), { key: 'Escape' });
  expect(screen.queryByRole('menu')).toBeNull(); expect(trigger()).toHaveFocus();
 });
 it('uses the phone View options sheet, preserves its controls and returns focus', async () => {
  viewport.isMobile = true;
  const p = props({ mobile: true, selection: { kind: 'workspace', id: workspaces[0].id } });
  render(AgendaToolbar, { props: p });
  const view = screen.getByRole('button', { name: 'View options: Workspace 00' }); view.focus(); await fireEvent.click(view);
  const sheet = screen.getByRole('dialog', { name: 'View options' }); await fireEvent.click(chooser());
  await waitFor(() => expect(sheet).toHaveFocus());
  expect(screen.getByRole('menuitemradio', { name: 'Show completed' })).toBeTruthy(); expect(screen.getByRole('menuitem', { name: /Time zone/ })).toBeTruthy();
  back().focus(); await fireEvent.keyDown(back(), { key: 'ArrowDown' }); expect(screen.getAllByRole('menuitemradio')[0]).toHaveFocus();
  await fireEvent.keyDown(document.activeElement!, { key: 'Escape' }); expect(view).toHaveFocus();
  await fireEvent.click(view); expect(chooser()).toBeTruthy(); await fireEvent.click(chooser());
  await fireEvent.click(screen.getByRole('menuitemradio', { name: /Workspace 29/ })); expect(p.onworkspace).toHaveBeenCalledWith(workspaces[29].id); expect(view).toHaveFocus();
 });
 it.each([
  { workspaces: null, workspacesLoading: true, message: 'Loading workspaces…' },
  { workspaces: [], message: 'No workspaces available.' },
  { workspaces, workspacesError: true, message: 'Could not load workspaces.' },
 ])('shows list state $message and keeps Back available', async extra => {
  const p = props(extra); render(AgendaToolbar, { props: p }); await fireEvent.click(trigger()); await fireEvent.click(chooser());
  expect(screen.getByText(extra.message)).toBeTruthy(); expect(back()).toBeTruthy();
  if (extra.workspacesError) { expect(screen.getAllByRole('menuitemradio')).toHaveLength(30); await fireEvent.click(screen.getByRole('menuitem', { name: 'Retry' })); expect(p.onretryworkspaces).toHaveBeenCalledOnce(); }
  else expect(screen.queryAllByRole('menuitemradio')).toHaveLength(0);
 });
 it('shows full duplicate labels, identity text, and only the selected workspace checkmark', async () => {
  const listed = [{ ...workspaces[0], name: 'Roadmap', slug: 'roadmap' }, { ...workspaces[1], name: 'Roadmap', slug: 'roadmap', owner_username: 'bob' }];
  render(AgendaToolbar, { props: props({ workspaces: listed, workspaceLabel: (ws: Workspace) => `${ws.name} · @${ws.owner_username}`, selection: { kind: 'workspace', id: listed[1].id } }) });
  expect(trigger()).toHaveTextContent('Roadmap · @bob'); await fireEvent.click(trigger());
  expect(screen.getAllByRole('menuitemradio').every(el => el.getAttribute('aria-checked') === 'false')).toBe(true);
  expect(chooser()).toHaveTextContent('Roadmap · @bob'); await fireEvent.click(chooser());
  const rows = screen.getAllByRole('menuitemradio'); expect(rows[0]).toHaveAttribute('aria-checked', 'false'); expect(rows[1]).toHaveAttribute('aria-checked', 'true');
  expect(rows[1]).toHaveAttribute('title', 'Roadmap · @bob'); expect(rows[1]).toHaveTextContent('@bob/roadmap');
 });
});
