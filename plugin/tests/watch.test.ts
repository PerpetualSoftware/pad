import { expect, mock, test } from 'claude-code/testing'

// TASK-3513: live item events start from the mod, only for a session that has
// consented (PLAN-2613's `pad session should-arm`), and an unarmed session runs
// nothing and shows nothing. These tests stand in for the CLI and the stream.

type Opts = { armed?: boolean; noCli?: boolean; chunks?: { stream: string; text: string }[] }

function stubs(on: any, opts: Opts = {}) {
  const runs: { argv: string[]; env: Record<string, string> }[] = []
  const spawns: { argv: string[]; env: Record<string, string> }[] = []
  const submitted: string[] = []
  const shown: string[] = []
  const stream = { closed: false, release: () => {} }
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
    if (e.argv.join(' ') === 'pad session should-arm') return { value: { exitCode: opts.armed ? 0 : 1, stdout: '', stderr: '' } }
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
  on('prompt.submit', ($: any, e: any) => { submitted.push(e.text); return { text: e.text } })
  return { runs, spawns, submitted, shown, stream }
}

const settle = () => new Promise((r) => setTimeout(r, 30))

test('an unarmed session registers its presence and starts and shows nothing', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
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
  await settle()
  expect(s.spawns).toEqual([])
  expect(s.shown).toEqual([])
})

test('the CLI calls name the harness session, which Claude Code does not export to a mod', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
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
  await settle()
  await $.session.end({ reason: 'prompt_input_exit' })
  s.stream.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.stream.closed).toBe(true)
  expect(s.submitted).toEqual([])
})
