import { expect, mock, test } from 'claude-code/testing'
import { charIndexAtByte, clipLine, displayLine, isPersonPrompt } from '../hooks/register.js'

// TASK-4071: chat delivered to the agent is shown to the person too. `pad watch`
// appends each chat line to ~/.pad/chat-seen/<harness pid>.log; the mod tails
// it into a band above the prompt and a toast per burst. These tests stand in
// for the CLI, the shell and the file.

const LOG = '/home/t/.pad/chat-seen/4242.log'
const DIR = '/home/t/.pad/chat-seen'

// What the log's stat reports: UTF-8 bytes, not characters.
function utf8Bytes(s: string) {
  let n = 0
  for (const ch of s) {
    const cp = ch.codePointAt(0) as number
    n += cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4
  }
  return n
}

const BAND = {
  plugin: 'pad',
  component: 'AbovePrompt',
  surface: 'terminal',
  viewport: { columns: 120, rows: 40 },
  props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 115, scroll: { offset: 0, bodyRows: 10 }, view: {} },
} as const

function stubs(on: any, initial: string | null = null, opts: { failReads?: number; failStats?: number; holdHome?: Promise<void>; growOnFirstRead?: string; holdFirstRead?: Promise<void> } = {}) {
  const file = { text: initial as string | null, isLink: false }
  let failReads = opts.failReads || 0
  const toasts: string[] = []
  const statted: string[] = []
  on('tool.call', () => ({ result: { stdout: '', stderr: '' }, text: '' }))
  on('session.start', ($: any, e: any) => ({ cwd: e.cwd }))
  on('session.end', () => ({ sessionId: 's1' }))
  on('command.register', () => ({ value: undefined }))
  on('store.get', () => ({ value: undefined }))
  on('ui.panes', () => ({ value: [] }))
  // The engine's own band beneath the mod: empty.
  on('ui.render', ($: any, e: any) => $.ui.resolve(e).Text({ children: ['engine'] }))
  on('ui.toast', ($: any, e: any) => { toasts.push(String(e.text)); return { value: undefined } })
  for (const name of ['ui.log', 'ui.status', 'ui.notice']) on(name, () => ({ value: undefined }))
  on('process.run', ($: any, e: any) => {
    const argv = e.argv.join(' ')
    if (argv.includes('$PPID')) return { value: { exitCode: 0, stdout: '4242\n', stderr: '' } }
    if (argv.includes('$HOME')) {
      const answer = { value: { exitCode: 0, stdout: '/home/t', stderr: '' } }
      return opts.holdHome ? opts.holdHome.then(() => answer) : answer
    }
    // pad session register / should-arm: unarmed, so no stream is spawned and
    // the reader is what is under test.
    return { value: { exitCode: 1, stdout: '', stderr: '' } }
  })
  let failStats = opts.failStats || 0
  on('fs.stat', ($: any, e: any) => {
    statted.push(e.path)
    if (failStats > 0) { failStats--; return { deny: 'EIO' } }
    if (e.path !== LOG || file.text === null) return { deny: 'ENOENT' }
    return { value: { kind: 'file', size: utf8Bytes(file.text), mtimeMs: 0, isLink: file.isLink } }
  })
  on('fs.list', ($: any, e: any) => {
    if (e.path !== DIR) return { deny: 'ENOENT' }
    return { value: file.text === null ? [] : [{ name: '4242.log', kind: file.isLink ? 'other' : 'file', size: 0, mtimeMs: 0 }] }
  })
  on('fs.read', ($: any, e: any) => {
    if (e.path !== LOG || file.text === null) return { deny: 'ENOENT' }
    if (failReads > 0) { failReads--; return { deny: 'EIO' } }
    if (opts.growOnFirstRead !== undefined) {
      // a line the writer appends between the reader's stat and its read
      file.text += opts.growOnFirstRead
      opts.growOnFirstRead = undefined
    }
    if (opts.holdFirstRead) {
      const held = opts.holdFirstRead
      opts.holdFirstRead = undefined
      const text = file.text
      return held.then(() => ({ value: text }))
    }
    return { value: file.text }
  })
  return { file, toasts, statted }
}

const settle = () => new Promise((r) => setTimeout(r, 30))
const usePad = ($: any) => $.tool.call({ tool: 'Bash', command: 'pad bootstrap --format json' })

async function bandTexts($: any) {
  const ui = await $.ui.mount(BAND)
  const found = await ui.findAll({ type: "Text" })
  await ui.unmount()
  return found.map((t: any) => t.text).filter((t: string) => t !== 'engine')
}

// One poll, then the burst's quiet: a toast shows once a burst ends.
async function poll(clock: any) {
  await clock.advance(1000)
  await settle()
  await clock.advance(1500)
  await settle()
}

// One poll inside a burst.
async function pollQuick(clock: any) {
  await clock.advance(1000)
  await settle()
}

test('starts at the end of the log: lines already there are never shown', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old line from a past session\n')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  expect(s.statted).toContain(LOG)
  await poll(clock)
  expect(s.toasts).toEqual([])
  expect(await bandTexts($)).toEqual([])
})

test('a new chat line shows in the band and as a toast', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old\n')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text += 'Message from rook via Pad: hello\n'
  await poll(clock)
  expect(s.toasts).toEqual(['Message from rook via Pad: hello'])
  const texts = await bandTexts($)
  expect(texts).toContain('Message from rook via Pad: hello')
  expect(texts.some((t: string) => /Pad chat/.test(t))).toBe(true)
})

test('a burst is one toast, and the band keeps the last three lines', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text += 'one\ntwo\n'
  await pollQuick(clock)
  expect(s.toasts).toEqual([])
  s.file.text += 'three\nfour\nfive\n'
  await poll(clock)
  expect(s.toasts).toEqual(['five (+4 more)'])
  const texts = await bandTexts($)
  expect(texts.filter((t: string) => !/Pad chat/.test(t))).toEqual(['three', 'four', 'five'])
})

test('a line still being written waits for its newline', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text += 'half a li'
  await poll(clock)
  expect(s.toasts).toEqual([])
  s.file.text += 'ne\n'
  await poll(clock)
  expect(s.toasts).toEqual(['half a line'])
})

test('a rotated log is read from its start (C3: lines rotated away between polls are lost)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'a long line that fills the log\n')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text = 'after rotation\n' // shorter: the writer moved the full log to .1
  await poll(clock)
  expect(s.toasts).toEqual(['after rotation'])
})

test('no log yet shows nothing; the first line written then shows', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, null)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await poll(clock)
  expect(s.toasts).toEqual([])
  s.file.text = 'first\n'
  await poll(clock)
  expect(s.toasts).toEqual(['first'])
})

test('a symlink at the log path is not read', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.isLink = true
  s.file.text = 'planted\n'
  await poll(clock)
  expect(s.toasts).toEqual([])
})

test('the reader survives /clear and /resume; any other end stops it', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await $.session.end({ reason: 'clear' })
  await $.session.end({ reason: 'resume' })
  s.file.text += 'still here\n'
  await poll(clock)
  expect(s.toasts).toEqual(['still here'])
  await $.session.end({ reason: 'prompt_input_exit' })
  s.file.text += 'after the end\n'
  await poll(clock)
  expect(s.toasts).toEqual(['still here'])
  expect(await bandTexts($)).toEqual([])
})

test('nothing reads the log before the session first uses Pad (TASK-3970)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await settle()
  s.file.text += 'early\n'
  await poll(clock)
  expect(s.statted).toEqual([])
  expect(s.toasts).toEqual([])
})

test('the band stays through a prompt the person did not type', async ($, on) => {
  const clock = mock.clock(on)
  on('prompt.submit', ($: any, e: any) => ({ text: e.text }))
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text += 'kept\n'
  await poll(clock)
  await $.prompt.submit({ text: 'relayed by a plugin' })
  await settle()
  expect(await bandTexts($)).toContain('kept')
})

test('only the person’s own prompt clears the band', () => {
  expect(isPersonPrompt({ origin: { kind: 'composer' } })).toBe(true)
  expect(isPersonPrompt({ origin: { kind: 'bridge' } })).toBe(true)
  for (const kind of ['plugin', 'task', 'peer', 'sdk', 'unclassified']) {
    expect(isPersonPrompt({ origin: { kind } })).toBe(false)
  }
  expect(isPersonPrompt({})).toBe(false)
})

test('control characters and direction overrides never reach the terminal', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, '')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text += 'hi \u001b]52;c;ZXZpbA==\u0007 there \u202egnp.exe\u009b\n'
  await poll(clock)
  expect(s.toasts).toEqual(['hi ]52;c;ZXZpbA== there gnp.exe'])
  expect(await bandTexts($)).toContain('hi ]52;c;ZXZpbA== there gnp.exe')
})

test('displayLine and clipLine', () => {
  expect(displayLine('a\u001b[31mb\u007fc\u200fd\u2066e\u061cf')).toBe('a[31mbcdef')
  const mixed = 'aé😀b' // UTF-8 bytes: 1, 2, 4, 1
  expect(charIndexAtByte(mixed, 3)).toBe(2)
  expect(charIndexAtByte(mixed, 7)).toBe(4)
  expect(charIndexAtByte(mixed, 8)).toBe(5)
  expect(clipLine('😀😀😀', 2)).toBe('😀…')
  expect(clipLine('a😀b', 3)).toBe('a😀b')
})

test('a session that ends while the reader is starting reads nothing', async ($, on) => {
  const clock = mock.clock(on)
  let release = () => {}
  const holdHome = new Promise<void>((r) => { release = r })
  const s = stubs(on, '', { holdHome })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await $.session.end({ reason: 'prompt_input_exit' })
  release()
  await settle()
  s.file.text += 'after the end\n'
  await poll(clock)
  await poll(clock)
  expect(s.statted).toEqual([])
  expect(s.toasts).toEqual([])
})

test('a first read that fails does not replay the log later', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old one\nold two\n', { failReads: 1 })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await poll(clock)
  s.file.text += 'new\n'
  await poll(clock)
  expect(s.toasts).toEqual(['new'])
})

test('a line written between the first stat and the first read still shows', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old\n', { growOnFirstRead: 'arrived during the first look\n' })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await poll(clock)
  expect(s.toasts).toEqual(['arrived during the first look'])
})

test('a session that ends during a poll can read again after its next wake', async ($, on) => {
  const clock = mock.clock(on)
  let release = () => {}
  const holdFirstRead = new Promise<void>((r) => { release = r })
  const s = stubs(on, 'old\n', { holdFirstRead })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await $.session.end({ reason: 'prompt_input_exit' })
  release()
  await settle()
  await usePad($)
  await settle()
  s.file.text += 'after the second wake\n'
  await poll(clock)
  expect(s.toasts).toEqual(['after the second wake'])
})

test('a moment the log cannot be read does not replay it', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old one\nold two\n')
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.isLink = true
  await poll(clock)
  s.file.isLink = false
  s.file.text += 'new\n'
  await poll(clock)
  expect(s.toasts).toEqual(['new'])
})

test('a first look that only failed does not start the log at zero (r3)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'old one\nold two\n', { failStats: 1 })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await poll(clock)
  s.file.text += 'new\n'
  await poll(clock)
  expect(s.toasts).toEqual(['new'])
})

test('a rotation that shrinks the log in bytes shows the new log whole (r3)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'é'.repeat(20) + '\n') // 41 bytes, 21 characters
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text = 'abcdefghijklmnopqrstuvwxyz0123\n' // 31 bytes, 31 characters
  await poll(clock)
  expect(s.toasts).toEqual(['abcdefghijklmnopqrstuvwxyz0123'])
})

test('a rotation that shrinks the log in characters shows the new log whole (r3)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, 'abcdefghijklmnopqrstuvwxyz0123\n') // 31 bytes, 31 characters
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.file.text = 'é'.repeat(20) + '\n' // 41 bytes, 21 characters
  await poll(clock)
  expect(s.toasts).toEqual(['é'.repeat(20)])
})
