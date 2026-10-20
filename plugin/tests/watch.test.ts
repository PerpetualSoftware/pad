import { expect, mock, test } from 'claude-code/testing'

// TASK-3513: live item events start from the mod, only for a session that has
// consented (PLAN-2613's `pad session should-arm`), and an unarmed session runs
// nothing and shows nothing. These tests stand in for the CLI and the stream.
// TASK-3970: none of it runs at session start; the session's first use of Pad
// (a pad command, a Pad MCP tool, the pane) starts it.

type Opts = { armed?: boolean; noCli?: boolean; chunks?: { stream: string; text: string }[]; gate?: Promise<void>; holdSubmit?: boolean }

function stubs(on: any, opts: Opts = {}) {
  const runs: { argv: string[]; env: Record<string, string> }[] = []
  const spawns: { argv: string[]; env: Record<string, string> }[] = []
  const submitted: string[] = []
  const shown: string[] = []
  const stream = { closed: false, release: () => {} }
  const atCall: string[][] = [] // the CLI calls already made when each tool call ran
  on('tool.call', () => {
    atCall.push(runs.map((r) => r.argv.join(' ')))
    return { result: { stdout: '', stderr: '' }, text: '' }
  })
  on('session.start', ($: any, e: any) => ({ cwd: e.cwd }))
  on('session.end', () => ({ sessionId: 's1' }))
  on('command.register', () => ({ value: undefined }))
  on('store.get', () => ({ value: undefined }))
  on('ui.panes', () => ({ value: [] }))
  for (const name of ['ui.log', 'ui.toast', 'ui.status', 'ui.notice']) {
    on(name, ($: any, e: any) => { shown.push(name + ': ' + String(e.text)); return { value: undefined } })
  }
  on('process.run', ($: any, e: any) => {
    runs.push({ argv: [...e.argv], env: { ...(e.init?.env || {}) } })
    if (e.argv[0] === 'sh') return { value: { exitCode: 0, stdout: '4242\n', stderr: '' } }
    if (opts.noCli) return { deny: 'spawn pad ENOENT' }
    if (e.argv.join(' ') === 'pad session should-arm') {
      const answer = { value: { exitCode: opts.armed ? 0 : 1, stdout: '', stderr: '' } }
      return opts.gate ? opts.gate.then(() => answer) : answer
    }
    return { value: { exitCode: 0, stdout: '', stderr: '' } }
  })
  on('process.spawn', async function* ($: any, e: any) {
    spawns.push({ argv: [...e.argv], env: { ...(e.env || {}) } })
    try {
      for (const c of opts.chunks || []) yield c
      // The stream stays open; each release() makes it write one more line.
      // An iterator the mod ended stops at that yield (an async generator's
      // return() takes effect at its next yield; the engine kills the child).
      for (;;) {
        await new Promise<void>((r) => { stream.release = r })
        yield { stream: 'stdout', text: 'late line\n' }
      }
    } finally {
      stream.closed = true
    }
  })
  const held = { release: () => {} }
  on('prompt.submit', ($: any, e: any) => {
    submitted.push(e.text)
    if (!opts.holdSubmit) return { text: e.text }
    return new Promise((r) => { held.release = () => r({ text: e.text }) })
  })
  return { runs, spawns, submitted, shown, stream, held, atCall }
}

const settle = () => new Promise((r) => setTimeout(r, 30))

// The session's first use of Pad: a pad command through Bash.
const usePad = ($: any, command = 'pad bootstrap --format json') => $.tool.call({ tool: 'Bash', command })

test('session start runs nothing: no process, no stream, no timer (TASK-3970)', async ($, on) => {
  // No mock.clock here: the test's own clock hooks record every timer asked for.
  const timers: string[] = []
  on('clock.every', ($: any, e: any) => { timers.push('every ' + e.ms); return new Promise(() => {}) })
  on('clock.after', ($: any, e: any) => { timers.push('after ' + e.ms); return new Promise(() => {}) })
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await settle()
  expect(s.runs).toEqual([])
  expect(s.spawns).toEqual([])
  expect(timers).toEqual([])
  expect(s.shown).toEqual([])
})

test('a Bash command that is not pad wakes nothing', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'ls -la && git status')
  await usePad($, 'cat ~/.padrc; echo spad')
  await settle()
  expect(s.runs).toEqual([])
  expect(s.spawns).toEqual([])
})

test('the first pad use wakes the session once; later ones run nothing more', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'cd /work/demo && pad item list tasks')
  await settle()
  await usePad($, '~/.local/bin/pad project next')
  await settle()
  const pad = s.runs.filter((r) => r.argv[0] === 'pad').map((r) => r.argv.join(' '))
  expect(pad).toEqual(['pad session register', 'pad session should-arm'])
})

test('presence is recorded before the first pad command runs: its worker id rides on that call', async ($, on) => {
  mock.clock(on)
  let release = () => {}
  const gate = new Promise<void>((r) => { release = r })
  const s = stubs(on, { armed: false, gate })
  await $.session.start({ cwd: '/work/demo' })
  const call = usePad($, 'pad item claim TASK-5')
  await settle()
  // The call ran after register, and while should-arm was still held open by
  // the gate: it waits for presence, never for the arm check.
  expect(s.atCall.length).toBe(1)
  expect(s.atCall[0]).toContain('pad session register')
  release()
  await call
})

test('pad run through a shell string or after an assignment wakes the session too', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  const commands = [`bash -c "pad item list"`, `sh -c 'pad project next'`, `PAD_AGENT=x pad bootstrap`, `X="$(pad auth whoami)"`]
  for (const command of commands) {
    await usePad($, command)
    await settle()
    // an end that stops the stream resets wake, so each command is a first use
    await $.session.end({ reason: 'prompt_input_exit' })
  }
  expect(s.runs.filter((r) => r.argv.join(' ') === 'pad session register').length).toBe(commands.length)
})

test('a Pad MCP tool call wakes the session too', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await $.tool.call({ tool: 'mcp__pad__pad_item', action: 'list' } as any)
  await settle()
  expect(s.runs.map((r) => r.argv.join(' '))).toContain('pad session should-arm')
  expect(s.spawns.length).toBe(1)
})

test('an unarmed session, once it uses pad, registers its presence and starts and shows nothing', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  expect(s.runs.map((r) => r.argv.join(' '))).toContain('pad session register')
  expect(s.runs.map((r) => r.argv.join(' '))).toContain('pad session should-arm')
  expect(s.spawns).toEqual([])
  expect(s.submitted).toEqual([])
  expect(s.shown).toEqual([])
})

test('without the pad CLI nothing starts and nothing shows', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { noCli: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  expect(s.spawns).toEqual([])
  expect(s.shown).toEqual([])
})

test('the CLI calls name the harness session, which Claude Code does not export to a mod', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  const pad = s.runs.filter((r) => r.argv[0] === 'pad')
  expect(pad.length).toBe(2)
  for (const r of pad) expect(r.env).toEqual({ CLAUDECODE: '1', PAD_SESSION_PID: '4242' })
  expect(s.spawns[0].env.PAD_SESSION_PID).toBe('4242')
})

test('an armed session starts the gated wrapper and relays its lines, one message per burst', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, {
    armed: true,
    chunks: [
      { stream: 'stdout', text: 'TASK-7 changed: status done\nTASK-8 ' },
      { stream: 'stderr', text: 'reconnecting\n' },
      { stream: 'stdout', text: 'pushed: look at this\n' },
    ],
  })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  expect(s.spawns.length).toBe(1)
  expect(s.spawns[0].argv[0]).toMatch(/\/scripts\/pad-monitor\.sh$/)
  expect(s.submitted).toEqual([])
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['TASK-7 changed: status done\nTASK-8 pushed: look at this'])
  expect(s.shown).toEqual([])
})

test('the stream survives /clear and /resume', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await $.session.end({ reason: 'clear' })
  await $.session.end({ reason: 'resume' })
  s.stream.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.stream.closed).toBe(false)
  expect(s.submitted).toEqual(['late line'])
})

test('any other end stops the stream: nothing after it is relayed', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await $.session.end({ reason: 'prompt_input_exit' })
  s.stream.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.stream.closed).toBe(true)
  expect(s.submitted).toEqual([])
})

test('a session that ends while the CLI is still answering starts nothing', async ($, on) => {
  mock.clock(on)
  let release = () => {}
  const gate = new Promise<void>((r) => { release = r })
  const s = stubs(on, { armed: true, gate })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await $.session.end({ reason: 'prompt_input_exit' })
  release()
  await settle()
  expect(s.spawns).toEqual([])
})

test('lines read after the session ended are not relayed by a flush already waiting', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, holdSubmit: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  await $.session.end({ reason: 'prompt_input_exit' })
  s.stream.release()
  s.held.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
})

test('a line arriving mid-burst joins it: the burst ends after quiet, not after its first line', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1000)
  s.stream.release() // 'late line', 1s into the burst
  await settle()
  await clock.advance(1000) // 2s after the first line, 1s after the last
  await settle()
  expect(s.submitted).toEqual([])
  await clock.advance(500)
  await settle()
  expect(s.submitted).toEqual(['first\nlate line'])
})
