// Engine polyfills for the headless materializer bundle (TASK-2198), evaluated
// before anything else in it. Each is a no-op where the host already provides
// the global: Node and jsdom (vitest) have all of them, and the Go runner
// (internal/materialize) installs Go-native atob, btoa, TextEncoder,
// TextDecoder and crypto.getRandomValues before the bundle runs, because the
// pure-JS forms were measurably slower under goja. The JS forms below stay as
// the fallback for any other host.
//
// Why each exists: entities' decode-data table uses atob when present and
// falls back to Buffer (goja failed at load with `Buffer is not defined`);
// lib0/webcrypto (yjs) reads crypto.subtle and crypto.getRandomValues at
// module load; cssomnom (linkedom's CSS layer) builds an
// Intl.NumberFormat("en-US", {useGrouping: false, ...}) at module load (goja
// failed at load with `Intl is not defined`).
const g = globalThis as any;
const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
if (typeof g.atob !== 'function') {
	g.atob = (s: string): string => {
		s = String(s).replace(/[\t\n\f\r =]/g, '');
		let out = '', acc = 0, bits = 0;
		for (let i = 0; i < s.length; i++) {
			const v = B64.indexOf(s[i]);
			if (v < 0) throw new Error('atob: invalid character');
			acc = (acc << 6) | v;
			bits += 6;
			if (bits >= 8) {
				bits -= 8;
				out += String.fromCharCode((acc >> bits) & 0xff);
			}
		}
		return out;
	};
}
// Only used for Y.Doc clientIDs / uuids; a materializer never writes.
if (typeof g.crypto === 'undefined') {
	g.crypto = {
		subtle: {},
		getRandomValues<T extends ArrayBufferView>(arr: T): T {
			const a = arr as unknown as { length: number; [i: number]: number };
			for (let i = 0; i < a.length; i++) a[i] = Math.floor(Math.random() * 0x100000000);
			return arr;
		},
	};
}
// goja regex bug, worked around rather than assumed fixed: "(text | image)*".split(/\s*(?=\b|\W|$)/) yields empty tokens
// (["(","text","","|","","image",")","*"]; V8 gives ["(","text","|","image",")","*"]),
// which breaks prosemirror-model's content-expression tokenizer at schema build.
// Install the ES2015 spec algorithm for RegExp.prototype[Symbol.split] only
// when the host fails that exact case.
if (JSON.stringify('(text | image)*'.split(/\s*(?=\b|\W|$)/)) !== '["(","text","|","image",")","*"]') {
	Object.defineProperty(RegExp.prototype, Symbol.split, {
		configurable: true,
		writable: true,
		value: function (this: RegExp, input: string, limit?: number): string[] {
			const S = String(input);
			const flags = this.flags.includes('y') ? this.flags : this.flags + 'y';
			const unicode = flags.includes('u');
			const splitter = new RegExp(this.source, flags);
			const A: string[] = [];
			const lim = limit === undefined ? 0xffffffff : limit >>> 0;
			if (lim === 0) return A;
			const size = S.length;
			if (size === 0) {
				splitter.lastIndex = 0;
				if (splitter.exec(S) !== null) return A;
				A.push(S);
				return A;
			}
			const advance = (i: number) => {
				if (!unicode || i + 1 >= size) return i + 1;
				const c = S.charCodeAt(i);
				if (c < 0xd800 || c > 0xdbff) return i + 1;
				const d = S.charCodeAt(i + 1);
				return d < 0xdc00 || d > 0xdfff ? i + 1 : i + 2;
			};
			let p = 0;
			let q = 0;
			while (q < size) {
				splitter.lastIndex = q;
				const z = splitter.exec(S);
				if (z === null) {
					q = advance(q);
					continue;
				}
				const e = Math.min(splitter.lastIndex, size);
				if (e === p) {
					q = advance(q);
					continue;
				}
				A.push(S.slice(p, q));
				if (A.length === lim) return A;
				p = e;
				for (let i = 1; i < z.length; i++) {
					A.push(z[i]);
					if (A.length === lim) return A;
				}
				q = p;
			}
			A.push(S.slice(p, size));
			return A;
		},
	});
}
// Exactly the slice of Intl.NumberFormat cssomnom uses: en-US, no grouping,
// minimumFractionDigits 0, an integer maximumFractionDigits. Anything else
// throws, so a dependency that needs more fails the bundle load in tests
// instead of formatting silently wrong. Rounds half away from zero on the
// shortest round-trip decimal digits (what Intl does), not on the binary
// value: toFixed would give "0" for 5e-7 where Intl gives "0.000001".
if (typeof g.Intl === 'undefined') {
	const unsupported = (what: string): never => {
		throw new TypeError('Intl.NumberFormat polyfill: unsupported ' + what);
	};
	class NumberFormat {
		private readonly maxFD: number;
		constructor(locales?: unknown, options?: unknown) {
			const okLocale =
				locales === undefined ||
				locales === 'en-US' ||
				(Array.isArray(locales) && locales.length === 1 && locales[0] === 'en-US');
			if (!okLocale) unsupported('locale ' + JSON.stringify(locales));
			// Grouping is not implemented, so useGrouping: false must be explicit.
			if (options === null || typeof options !== 'object') unsupported('options (useGrouping: false is required)');
			const o = options as Record<string, unknown>;
			for (const k of Object.keys(o)) {
				if (k !== 'useGrouping' && k !== 'minimumFractionDigits' && k !== 'maximumFractionDigits') unsupported('option ' + k);
			}
			if (o.useGrouping !== false) unsupported('useGrouping ' + String(o.useGrouping));
			if (o.minimumFractionDigits !== undefined && o.minimumFractionDigits !== 0) {
				unsupported('minimumFractionDigits ' + String(o.minimumFractionDigits));
			}
			const max = o.maximumFractionDigits === undefined ? 3 : o.maximumFractionDigits;
			if (typeof max !== 'number' || !Number.isInteger(max) || max < 0 || max > 20) {
				unsupported('maximumFractionDigits ' + String(max));
			}
			this.maxFD = max as number;
		}
		format(value: unknown): string {
			const x = Number(value);
			if (Number.isNaN(x)) return 'NaN';
			const sign = x < 0 || Object.is(x, -0) ? '-' : '';
			if (!Number.isFinite(x)) return sign + '∞';
			// Shortest round-trip digits, exponent form expanded: the digit
			// string plus how many of them precede the decimal point (may be
			// <= 0 or exceed its length).
			let s = String(Math.abs(x));
			let exp = 0;
			const e = s.indexOf('e');
			if (e >= 0) {
				exp = Number(s.slice(e + 1));
				s = s.slice(0, e);
			}
			const dot = s.indexOf('.');
			const digits = dot < 0 ? s : s.slice(0, dot) + s.slice(dot + 1);
			const intLen = (dot < 0 ? s.length : dot) + exp;
			let int: string, frac: string;
			if (intLen <= 0) {
				int = '0';
				frac = '0'.repeat(-intLen) + digits;
			} else if (intLen >= digits.length) {
				int = digits + '0'.repeat(intLen - digits.length);
				frac = '';
			} else {
				int = digits.slice(0, intLen);
				frac = digits.slice(intLen);
			}
			const n = this.maxFD;
			if (frac.length > n) {
				const up = frac.charCodeAt(n) >= 53; // '5' or above
				frac = frac.slice(0, n);
				if (up) {
					const d = (int + frac).split('');
					let i = d.length - 1;
					while (i >= 0 && d[i] === '9') d[i--] = '0';
					if (i >= 0) d[i] = String.fromCharCode(d[i].charCodeAt(0) + 1);
					else d.unshift('1');
					const all = d.join('');
					int = all.slice(0, all.length - n);
					frac = all.slice(all.length - n);
				}
			}
			int = int.replace(/^0+(?=\d)/, '');
			frac = frac.replace(/0+$/, '');
			return sign + int + (frac ? '.' + frac : '');
		}
		resolvedOptions() {
			return {
				locale: 'en-US',
				numberingSystem: 'latn',
				style: 'decimal',
				useGrouping: false,
				minimumFractionDigits: 0,
				maximumFractionDigits: this.maxFD,
			};
		}
	}
	g.Intl = { NumberFormat };
}
if (typeof g.btoa !== 'function') {
	g.btoa = (s: string): string => {
		let out = '';
		for (let i = 0; i < s.length; i += 3) {
			const a = s.charCodeAt(i), b = s.charCodeAt(i + 1), c = s.charCodeAt(i + 2);
			const n = (a << 16) | ((b || 0) << 8) | (c || 0);
			out += B64[(n >> 18) & 63] + B64[(n >> 12) & 63] + (i + 1 < s.length ? B64[(n >> 6) & 63] : '=') + (i + 2 < s.length ? B64[n & 63] : '=');
		}
		return out;
	};
}

export {};
