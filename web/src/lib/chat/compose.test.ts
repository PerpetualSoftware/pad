// TASK-4044 PR 3b: what the composer may send. Each rule is the server's
// (internal/server/handlers_chat.go:243-267), pinned with the inputs that tell
// the server's rule from the nearest JS one.
import { describe, expect, it } from 'vitest';
import {
	MAX_BODY_BYTES,
	MAX_REQUEST_BYTES,
	PLACEHOLDER_OP_KEY,
	isRequestTooLargeError,
	utf8Bytes,
	validateDraft
} from './compose';

const check = (body: string, reply?: number) => validateDraft(body, reply, PLACEHOLDER_OP_KEY);

describe('validateDraft', () => {
	// N1: Go's TrimSpace, not JS trim(). U+0085 is space to Go and not to JS; U+FEFF the reverse.
	it('is empty for what Go trims, and not for what only JS trims', () => {
		expect(check('')).toBe('empty');
		expect(check(' \t\n　')).toBe('empty');
		expect(check('\u0085')).toBe('empty');
		expect(check('﻿')).toBeNull();
	});

	// N2: the cap counts UTF-8 bytes. 20,000 emoji are 40,000 UTF-16 units and 80,000 bytes.
	it('counts UTF-8 bytes against the body cap', () => {
		expect(utf8Bytes('😀')).toBe(4);
		expect(check('😀'.repeat(20_000))).toBe('too_long');
		expect(check('a'.repeat(MAX_BODY_BYTES))).toBeNull();
		expect(check('a'.repeat(MAX_BODY_BYTES + 1))).toBe('too_long');
	});

	// N3: the 128 KiB request cap is on the escaped JSON, so a body within the body cap can exceed it.
	it('refuses a body whose escaped JSON passes the request cap', () => {
		expect(check('"'.repeat(MAX_BODY_BYTES))).toBe('too_large');
		expect(check('\u0001'.repeat(22_000))).toBe('too_large');
	});

	it('draws the request cap at exactly 128 KiB of payload', () => {
		const overhead = utf8Bytes(JSON.stringify({ body: '', op_key: PLACEHOLDER_OP_KEY }));
		const room = MAX_REQUEST_BYTES - overhead;
		const body = 'a'.repeat(room % 2) + '"'.repeat(Math.floor(room / 2));
		expect(utf8Bytes(JSON.stringify({ body, op_key: PLACEHOLDER_OP_KEY }))).toBe(MAX_REQUEST_BYTES);
		expect(check(body)).toBeNull();
		expect(check(body + 'a')).toBe('too_large');
	});

	it('counts reply_to_seq in the payload', () => {
		const overhead = utf8Bytes(JSON.stringify({ body: '', reply_to_seq: 7, op_key: PLACEHOLDER_OP_KEY }));
		const room = MAX_REQUEST_BYTES - overhead;
		const body = 'a'.repeat(room % 2) + '"'.repeat(Math.floor(room / 2));
		expect(check(body, 7)).toBeNull();
		expect(check(body + 'a', 7)).toBe('too_large');
	});
});

describe('isRequestTooLargeError', () => {
	it('matches the server 400 for an over-size request and nothing else', () => {
		const e = (code: string, message: string) => Object.assign(new Error(message), { code });
		expect(isRequestTooLargeError(e('validation_error', 'Invalid JSON body (maximum 128 KiB)'))).toBe(true);
		expect(isRequestTooLargeError(e('validation_error', 'body is required'))).toBe(false);
		expect(isRequestTooLargeError(e('forbidden', 'Invalid JSON body (maximum 128 KiB)'))).toBe(false);
		expect(isRequestTooLargeError(null)).toBe(false);
	});
});
