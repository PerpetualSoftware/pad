/** Paths rendered without the workspace frame, including legacy account links. */
export function isAccountShellPath(pathname: string): boolean {
	const path = pathname.replace(/\/+$/, '') || '/';
	return path === '/' || path === '/needs' || path === '/agenda' ||
		['/tutorials', '/settings', '/console'].some(prefix => path === prefix || path.startsWith(prefix + '/'));
}
