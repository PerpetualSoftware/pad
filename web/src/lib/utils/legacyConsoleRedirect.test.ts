import { describe, expect, it } from 'vitest';
import { legacyConsoleTarget } from './legacyConsoleRedirect';

describe('legacyConsoleTarget', () => {
 const routes = [
  ['/console', '/'], ['/console/', '/'],
  ['/console/needs', '/needs'], ['/console/agenda', '/agenda'],
  ['/console/tutorials', '/tutorials'], ['/console/tutorials/start', '/tutorials/start'],
  ['/console/settings', '/settings/account'],
  ['/console/connected-apps', '/settings/connected-apps'],
  ['/console/billing', '/settings/billing'],
  ['/console/deleted-workspaces', '/settings/deleted-workspaces'],
  ['/console/admin', '/settings/admin'],
  ['/console/admin/invitations', '/settings/admin/invitations'],
  ['/console/admin/audit-log', '/settings/admin/audit-log'],
  ['/console/admin/mcp-audit', '/settings/admin/mcp-audit'],
  ['/console/admin/billing', '/settings/admin/billing'],
  ['/console/admin/settings', '/settings/admin/instance'],
  ['/console/move-to-cloud', '/settings/move-to-cloud'],
  ['/console/zzz', '/'], ['/console/admin/zzz', '/'],
 ];
 it.each(routes)('maps %s preserving query bytes and hash', (from, to) => {
  expect(legacyConsoleTarget(from, '?q=1&encoded=%2f+a', '#h')).toBe(to + '?q=1&encoded=%2f+a#h');
  expect(legacyConsoleTarget(from + '/', '?q=1', '#h')).toBe(to + '?q=1#h');
 });
 it.each(['', '?x=1'])('opens create with query %s and hash', (search) => {
  expect(legacyConsoleTarget('/console/new', search, '#h')).toBe('/?openCreate=1' + (search ? '&x=1' : '') + '#h');
 });
 it.each(['chat', 'notifications', 'calendar-feeds'])('preserves the %s anchor', (anchor) => {
  expect(legacyConsoleTarget('/console/settings', '', '#' + anchor)).toBe('/settings/account#' + anchor);
 });
});
