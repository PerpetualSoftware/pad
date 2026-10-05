/**
 * The "via <App>" attribution label (SPEC-6 U9c, TASK-3413; Dave §11 Q4).
 * A write an installed app made, or a person made through one, keeps its
 * author; the app is a quiet secondary label beside it, never a second author
 * line. The server resolves the name (the app's bot name, else its origin),
 * and keeps it after the app is uninstalled.
 */
export function viaAppTitle(name: string): string {
	return `Written through the installed app ${name}`;
}
