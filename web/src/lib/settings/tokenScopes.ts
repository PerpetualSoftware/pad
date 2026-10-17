// What an API key's stored scopes let it do, in words (TASK-3866).
//
// This mirrors tokenScopeAllows (internal/server/middleware_auth.go)
// rather than reading the scope names at face value. The server sees two
// levels, read and write ("pad:admin" grants exactly what "write" does),
// and some stored values deny everything. The label must never say a key
// can do something the server would refuse. The shared fixture
// internal/server/testdata/token_scope_levels.json holds both sides to
// the same answers.

export type TokenAccessLevel = 'write' | 'read' | 'none';

export interface TokenAccess {
	level: TokenAccessLevel;
	label: string;
	/** "" or []: a key from before scopes were enforced, unrestricted. */
	legacy: boolean;
	/** Values the server ignores, in stored order. */
	unknown: string[];
}

const WRITE_SCOPES = new Set(['*', 'write', 'pad:write', 'pad:admin']);
const READ_SCOPES = new Set(['read', 'pad:read']);

// Go's unicode.IsSpace, which strings.TrimSpace uses for the server's
// ["*"] fast path. JS trim() differs (it trims U+FEFF and not U+0085), and
// the fast path is the only place the server trims anything, so a padded
// ["*"] is full access while a padded ["read"] fails to parse.
const GO_SPACE_CLASS = '[\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]';
const GO_SPACE = new RegExp(`^${GO_SPACE_CLASS}+|${GO_SPACE_CLASS}+$`, 'g');

const LABELS: Record<TokenAccessLevel, string> = {
	write: 'Read and write',
	read: 'Read only',
	none: 'No access',
};

function result(level: TokenAccessLevel, legacy = false, unknown: string[] = []): TokenAccess {
	return { level, label: LABELS[level], legacy, unknown };
}

// parseScopes returns what Go's json.Unmarshal into []string yields:
// undefined where it fails (deny), null for a JSON null (deny), else the
// strings, with a null element read as "" as Go reads it.
function parseScopes(raw: string): string[] | null | undefined {
	let parsed: unknown;
	try {
		parsed = JSON.parse(raw);
	} catch {
		return undefined;
	}
	if (parsed === null) return null;
	if (!Array.isArray(parsed)) return undefined;
	const out: string[] = [];
	for (const v of parsed) {
		if (v === null) out.push('');
		else if (typeof v === 'string') out.push(v);
		else return undefined;
	}
	return out;
}

export function describeTokenScopes(raw: string): TokenAccess {
	if (raw === '') return result('write', true);
	if (raw.replace(GO_SPACE, '') === '["*"]') return result('write');

	const scopes = parseScopes(raw);
	if (!scopes) return result('none');
	if (scopes.length === 0) return result('write', true);

	let level: TokenAccessLevel = 'none';
	const unknown: string[] = [];
	for (const s of scopes) {
		if (WRITE_SCOPES.has(s)) level = 'write';
		else if (READ_SCOPES.has(s)) {
			if (level === 'none') level = 'read';
		} else unknown.push(s);
	}
	return result(level, false, unknown);
}
