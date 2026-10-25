import { expect, mock, test } from 'claude-code/testing'
import { isPersonPrompt } from '../hooks/register.js'

// TASK-4071: chat delivered to the agent is shown to the person too. `pad watch`
// appends each chat line to ~/.pad/chat-seen/<harness pid>.log; the mod tails
// it into a band above the prompt and a toast per burst. These tests stand in
// for the CLI, the shell and the file.

const LOG = '/home/t/.pad/chat-seen/4242.log'

const BAND = {
  plugin: 'pad',
  component: 'AbovePrompt',
  surface: 'terminal',
  viewport: { columns: 120, rows: 40 },
  props: { hasSurvey: false, isWorking: false, maxRows: 10, bodyColumns: 115, scroll: { offset: 0, bodyRows: 10 }, view: {} },
} as const

function stubs(on: any, initial: string | null = null) {
  const file = { text: initial as string | null, isLink: false }
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
    if (argv.includes('$HOME')) return { value: { exitCode: 0, stdout: '/home/t', stderr: '' } }
    // pad session register / should-arm: unarmed, so no stream is spawned and
    // the reader is what is under test.
    return { value: { exitCode: 1, stdout: '', stderr: '' } }
  })
  on('fs.stat', ($: any, e: any) => {
    statted.push(e.path)
    if (e.path !== LOG || file.text === null) return { deny: 'ENOENT' }
    return { value: { kind: 'file', size: file.text.length, mtimeMs: 0, isLink: file.isLink } }
  })
  on('fs.read', ($: any, e: any) => {
    if (e.path !== LOG || file.text === null) return { deny: 'ENOENT' }
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

async function poll(clock: any) {
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
  await poll(clock)
  s.file.text += 'three\nfour\nfive\n'
  await poll(clock)
  expect(s.toasts).toEqual(['two (+1 more)', 'five (+2 more)'])
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
