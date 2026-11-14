/** Translate old account URLs without decoding or reserializing query/hash bytes. */
export function legacyConsoleTarget(pathname: string, search: string, hash: string): string {
	const path = pathname.replace(/\/+$/, '');
	const routes: Record<string, string> = {
		'/console': '/',
		'/console/needs': '/needs',
		'/console/agenda': '/agenda',
		'/console/tutorials': '/tutorials',
		'/console/settings': '/settings/account',
		'/console/connected-apps': '/settings/connected-apps',
		'/console/billing': '/settings/billing',
		'/console/deleted-workspaces': '/settings/deleted-workspaces',
		'/console/admin': '/settings/admin',
		'/console/admin/invitations': '/settings/admin/invitations',
		'/console/admin/audit-log': '/settings/admin/audit-log',
		'/console/admin/mcp-audit': '/settings/admin/mcp-audit',
		'/console/admin/billing': '/settings/admin/billing',
		'/console/admin/settings': '/settings/admin/instance',
		'/console/move-to-cloud': '/settings/move-to-cloud'
	};
	if (path === '/console/new') return '/?openCreate=1' + (search ? '&' + search.slice(1) : '') + hash;
	const tutorial = path.match(/^\/console\/tutorials\/([^/]+)$/);
	const target = tutorial ? '/tutorials/' + tutorial[1] : routes[path] ?? '/';
	return target + search + hash;
}

/** Both old and current workspace-list URLs mean no explicit auth destination. */
export function isDefaultAccountTarget(target: string): boolean {
	return target === '/' || target === '/console' || target === '/console/';
}
