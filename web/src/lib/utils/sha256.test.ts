import { describe, expect, it } from 'vitest';
import { sha256Hex } from './sha256';

// BUG-3124: the server compares this against Go's sha256.Sum256 over the UTF-8
// bytes of items.content, so every vector here is a reference digest computed
// outside this code (sha256sum / Python hashlib), not a value this function
// produced. The padding boundaries (55/56/63/64/119 bytes) are where a SHA-256
// implementation most often goes wrong.
describe('sha256Hex', () => {
	it.each([
		['', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
		['abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
		['a'.repeat(55), '9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318'],
		['a'.repeat(56), 'b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a'],
		['a'.repeat(63), '7d3e74a05d7db15bce4ad9ec0658ea98e3f06eeecf16b4c6fff2da457ddc2f34'],
		['a'.repeat(64), 'ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb'],
		['a'.repeat(119), '31eba51c313a5c08226adf18d4a359cfdfd8d2e816b13f4af952f7ea6584dcfb'],
		['a'.repeat(1000), '41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3'],
		// Multi-byte UTF-8 (2-, 3- and 4-byte sequences): hashing UTF-16 code
		// units instead of UTF-8 bytes would disagree with Go here.
		['héllo — 世界 🌍', 'e5cba050fccd5c0bb055ebef890b44f96a744a16e886fe39ef5d66e6dbe84362'],
	])('matches the reference digest for %j', (input, want) => {
		expect(sha256Hex(input)).toBe(want);
	});
});
