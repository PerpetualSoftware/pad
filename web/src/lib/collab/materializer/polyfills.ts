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
// module load.
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
