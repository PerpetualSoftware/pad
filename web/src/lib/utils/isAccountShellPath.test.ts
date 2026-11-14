import { expect, it } from 'vitest';
import { isAccountShellPath } from './isAccountShellPath';

it.each(['/', '/needs', '/needs/', '/agenda', '/tutorials', '/tutorials/start', '/settings', '/settings/account', '/settings/connected-apps', '/settings/billing', '/settings/deleted-workspaces', '/settings/admin', '/settings/admin/invitations', '/settings/admin/audit-log', '/settings/admin/mcp-audit', '/settings/admin/billing', '/settings/admin/instance', '/settings/move-to-cloud', '/console', '/console/settings'])('uses the account shell for %s', (path) => {
 expect(isAccountShellPath(path)).toBe(true);
});
it.each(['/alice/work/settings', '/alice/work', '/settingsfoo', '/tutorialsfoo', '/consolefoo', '/needs/other', '/agenda/other', '/login', '/messages'])('rejects non-account path %s', (path) => {
 expect(isAccountShellPath(path)).toBe(false);
});
