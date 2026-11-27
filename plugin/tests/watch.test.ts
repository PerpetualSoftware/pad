import { expect, mock, test } from 'claude-code/testing'

// TASK-3513: live item events start from the mod, only for a session that has
// consented (PLAN-2613's `pad session should-arm`), and an unarmed session runs
// nothing and shows nothing. These tests stand in for the CLI and the stream.
// TASK-3970: none of it runs at session start; the session's first use of Pad
// (a pad command, a Pad MCP tool, the pane) starts it.

type Opts = { onTool?: (e: any) => Promise<void> | void; armed?: boolean; noCli?: boolean; chunks?: { stream: string; text: string }[]; gate?: Promise<void>; holdSubmit?: boolean }

function stubs(on: any, opts: Opts = {}) {
  const runs: { argv: string[]; env: Record<string, string> }[] = []
  const spawns: { argv: string[]; env: Record<string, string> }[] = []
  const submitted: string[] = []
  const shown: string[] = []
  // code: a should-arm answer other than armed/not armed (a number), or a
  // run that throws ('throw'), as a timeout or a missing CLI would (BUG-3741).
  const consent: { armed: boolean; code?: number | 'throw' } = {
    get armed() { return Boolean(opts.armed) },
    set armed(v: boolean) { opts.armed = v },
  }
  const stream = { closed: false, release: () => {}, end: () => {} }
  const atCall: string[][] = [] // the CLI calls already made when each tool call ran
  on('tool.call', async ($: any, e: any) => {
    atCall.push(runs.map((r) => r.argv.join(' ')))
    if (opts.onTool) await opts.onTool(e)
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
      if (consent.code === 'throw') return { deny: 'pad session should-arm timed out' }
      if (typeof consent.code === 'number') return { value: { exitCode: consent.code, stdout: '', stderr: '' } }
      const answer = { value: { exitCode: consent.armed ? 0 : 1, stdout: '', stderr: '' } }
      return opts.gate ? opts.gate.then(() => answer) : answer
    }
    return { value: { exitCode: 0, stdout: '', stderr: '' } }
  })
  on('process.spawn', async function* ($: any, e: any) {
    spawns.push({ argv: [...e.argv], env: { ...(e.env || {}) } })
    stream.closed = false
    try {
      for (const c of opts.chunks || []) yield c
      // The stream stays open; each release() makes it write one more line,
      // and end() makes the stream exit on its own (BUG-3741).
      // An iterator the mod ended stops at that yield (an async generator's
      // return() takes effect at its next yield; the engine kills the child).
      for (;;) {
        const next = await new Promise<'line' | 'end'>((r) => {
          stream.release = () => r('line')
          stream.end = () => r('end')
        })
        if (next === 'end') return { value: { exitCode: 0 } }
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
  return { runs, spawns, submitted, shown, stream, held, atCall, consent }
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

// Which Bash commands count as a use of Pad: the population, not a sample
// (team CONVE-35; rounds r1 and r2 each found one more missed shape).
const RUNS_PAD = [
  'pad bootstrap', 'pad', '  pad item list', 'cd /w && pad item list', 'ls; pad project next', 'a || pad x', 'a | pad x',
  'bash -c "pad item list"', `sh -c 'pad project next'`, 'bash -c "pad"', `sh -c 'pad'`, 'echo $(pad)', 'X="$(pad auth whoami)"',
  'echo `pad auth whoami`', 'pad;ls', 'pad&&ls', '\\pad item list', 'PAD_AGENT=x pad bootstrap', '~/.local/bin/pad project next',
  '/usr/local/bin/pad item show TASK-5', './pad item list', '../bin/pad x', 'env pad bootstrap', 'time pad x', '(pad x)', '{ pad x; }', 'pad\titem list',
]
const NOT_PAD = ['ls -la && git status', 'cat ~/.padrc', 'echo spad', 'padd --version', 'ls pad-monitor.sh', 'grep -r ipad .', 'cat notepad.txt', 'echo pad_x']

test('every shape that runs pad wakes the session before that command runs (TASK-3970)', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  for (const command of RUNS_PAD) {
    const before = s.atCall.length
    await usePad($, command)
    await settle()
    // register ran before this command's call reached the tool
    expect([command, s.atCall[before].includes('pad session register')]).toEqual([command, true])
    // an end that stops the stream resets wake, so each command is a first use
    await $.session.end({ reason: 'prompt_input_exit' })
    s.runs.length = 0
  }
})

test('a command that does not run pad wakes nothing', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  for (const command of NOT_PAD) await usePad($, command)
  await settle()
  expect(s.runs).toEqual([])
})

// Pad MCP tools under any server name, and tools that are not Pad's.
const PAD_MCP = ['mcp__pad__pad_item', 'mcp__claude_ai_Pad__pad_project', 'mcp__plugin_pad_pad__pad_search', 'mcp__work-tracker__pad_collection']
const NOT_PAD_MCP = ['mcp__pad__other', 'mcp__ipad__notes', 'mcp__github__create_issue', 'mcp__notepad__pad']

test('a Pad MCP tool under any server name wakes the session; other tools do not', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  for (const tool of NOT_PAD_MCP) await $.tool.call({ tool } as any)
  await settle()
  expect(s.runs).toEqual([])
  for (const tool of PAD_MCP) {
    const before = s.atCall.length
    await $.tool.call({ tool } as any)
    await settle()
    expect([tool, s.atCall[before].includes('pad session register')]).toEqual([tool, true])
    await $.session.end({ reason: 'prompt_input_exit' })
    s.runs.length = 0
  }
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

// BUG-4303: the mod asks should-arm once, at the session's first use of Pad. A
// session that opts in LATER (/pad:connect, typed or reached by intent, runs
// `pad session arm` through Bash) used to have no path to a stream: it never
// appeared in Chat and got no pushes. The stream now starts when the arm
// command returns, in the same session.
const ARMS = [
  'pad session arm', 'pad session arm --format json', 'cd /work/demo && pad session arm', 'PAD_AGENT=x pad session arm',
  '/usr/local/bin/pad session arm', '~/.local/bin/pad  session   arm', `bash -c "pad session arm"`, 'pad session arm; pad session first-connect --format json',
  '(pad session arm)', 'pad\tsession\tarm',
]
const NOT_ARMS = [
  'pad session disarm', 'pad session status --format json', 'pad session should-arm', 'pad session first-connect --format json',
  'pad chat arm --accept-instructions', 'pad session armed', 'pad session arm-all', 'padd session arm', 'ls pad-session-arm',
]

test('a session that arms after its first use of Pad starts the stream when the arm returns (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const opts: Opts = { armed: false }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'pad bootstrap --format json') // the first use: not armed, so nothing starts
  await settle()
  expect(s.spawns).toEqual([])
  opts.armed = true // `pad session arm` takes effect
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns.length).toBe(1)
  expect(s.spawns[0].argv[0]).toMatch(/\/scripts\/pad-monitor\.sh$/)
  expect(s.spawns[0].env.PAD_SESSION_PID).toBe('4242')
})

test('every shape of the arm command starts the stream, and no other command does (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const opts: Opts = { armed: false }
  const s = stubs(on, opts)
  const tryCommand = async (command: string) => {
    opts.armed = false
    await $.session.start({ cwd: '/work/demo' })
    await usePad($, 'pad bootstrap') // the first use: not armed
    opts.armed = true // consent exists by the time the command returns
    await usePad($, command)
    await settle()
    const started = s.spawns.length
    // an end that stops the stream resets wake and the stream
    await $.session.end({ reason: 'prompt_input_exit' })
    s.spawns.length = 0
    s.runs.length = 0
    return [command, started]
  }
  for (const command of ARMS) expect(await tryCommand(command)).toEqual([command, 1])
  for (const command of NOT_ARMS) expect(await tryCommand(command)).toEqual([command, 0])
})

test('only a Bash command arms: the same text in another tool starts nothing (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const opts: Opts = { armed: false }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'pad bootstrap')
  opts.armed = true
  await $.tool.call({ tool: 'Write', command: 'pad session arm' })
  await settle()
  expect(s.spawns).toEqual([])
})

test('a second arm, or a reconnect, does not start a second stream (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const opts: Opts = { armed: false }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'pad bootstrap')
  opts.armed = true
  await usePad($, 'pad session arm')
  await usePad($, 'pad session arm')
  await settle()
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns.length).toBe(1)
})

test('two arms in flight at once start one stream (BUG-4303)', async ($, on) => {
  mock.clock(on)
  let release = () => {}
  const gate = new Promise<void>((r) => { release = r })
  const opts: Opts = { armed: false, gate }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  opts.armed = true
  const a = usePad($, 'pad session arm')
  const b = usePad($, 'pad session arm')
  await settle()
  release()
  await Promise.all([a, b])
  await settle()
  expect(s.spawns.length).toBe(1)
})

test('an arm that returns while the first-use check is still pending starts one stream (BUG-4303)', async ($, on) => {
  mock.clock(on)
  let release = () => {}
  const gate = new Promise<void>((r) => { release = r })
  const opts: Opts = { armed: true, gate }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  const first = usePad($, 'pad bootstrap') // wake asks should-arm; the gate holds the answer
  await settle()
  const arm = usePad($, 'pad session arm') // the post-arm check overlaps it
  await settle()
  release()
  await Promise.all([first, arm])
  await settle()
  expect(s.spawns.length).toBe(1)
})

test('an arm that did not take effect starts nothing (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: false })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns).toEqual([])
})

test('after a disconnect the mod starts nothing; a later connect does not double the stream it holds (BUG-4303)', async ($, on) => {
  mock.clock(on)
  const opts: Opts = { armed: false }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($, 'pad bootstrap')
  opts.armed = true
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns.length).toBe(1)
  // BUG-3741: `pad session disarm` ends the stream at once in the mod, so
  // disarm starts nothing and a later `pad session arm` starts a new stream.
  opts.armed = false
  await usePad($, 'pad session disarm')
  s.stream.release()
  await settle()
  expect(s.spawns.length).toBe(1)
  opts.armed = true
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns.length).toBe(2)
})

// BUG-3741: withdrawing consent or ending the stream drops queued lines and
// cancels any pending burst timer so nothing is relayed after consent ends.
test('queued lines are not submitted after the stream ends (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, holdSubmit: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  s.stream.release()
  await settle()
  await clock.advance(1500)
  await settle()
  s.consent.armed = false
  s.stream.end()
  await settle()
  s.held.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
})

test('consent withdrawn before the stream ends stops queued delivery on recheck (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, holdSubmit: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  const checksBefore = s.runs.filter((r) => r.argv.join(' ') === 'pad session should-arm').length
  s.stream.release()
  await settle()
  s.consent.armed = false
  s.held.release()
  await settle()
  await clock.advance(1500)
  await settle()
  const checksAfter = s.runs.filter((r) => r.argv.join(' ') === 'pad session should-arm').length
  expect(s.submitted).toEqual(['first'])
  expect(checksAfter).toBe(checksBefore + 1)
})

test('a burst timer scheduled before the stream ends does not flush (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  s.stream.end()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual([])
})

test('pad session disarm via Bash ends the stream and a later arm starts a new one (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  expect(s.spawns.length).toBe(1)
  s.stream.release()
  await settle()
  s.consent.armed = false
  await usePad($, 'pad session disarm')
  s.stream.release()
  await settle()
  expect(s.stream.closed).toBe(true)
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual([])
  s.consent.armed = true
  await usePad($, 'pad session arm')
  await settle()
  expect(s.spawns.length).toBe(2)
})

const DISARMS = [
  'pad session disarm',
  'cd x && pad session disarm',
  '/usr/local/bin/pad session disarm',
]
const NOT_DISARMS = [
  'pad session arm',
  'pad session disarmed',
  'padd session disarm',
]

test('every shape of the disarm command ends the stream, and no other command or tool does (BUG-3741)', async ($, on) => {
  mock.clock(on)
  const s = stubs(on, { armed: true })
  const tryCall = async (call: { tool: string; command?: string }) => {
    await $.session.start({ cwd: '/work/demo' })
    await usePad($)
    await settle()
    s.consent.armed = false // what a disarm that took effect leaves
    await $.tool.call(call as any)
    s.stream.release()
    await settle()
    const closed = s.stream.closed
    s.consent.armed = true
    await $.session.end({ reason: 'prompt_input_exit' })
    s.stream.release()
    await settle()
    return [call.tool + ':' + (call.command || ''), closed]
  }
  for (const command of DISARMS) {
    expect(await tryCall({ tool: 'Bash', command })).toEqual(['Bash:' + command, true])
  }
  for (const command of NOT_DISARMS) {
    expect(await tryCall({ tool: 'Bash', command })).toEqual(['Bash:' + command, false])
  }
  expect(await tryCall({ tool: 'Write', command: 'pad session disarm' })).toEqual(['Write:pad session disarm', false])
})

test('an armed session rechecks consent with the stream env and submits a burst once (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  const checks = s.runs.filter((r) => r.argv.join(' ') === 'pad session should-arm')
  expect(checks.length).toBe(2)
  expect(checks[1].env).toEqual({ CLAUDECODE: '1', PAD_SESSION_PID: '4242' })
})

// BUG-3741 r1: only should-arm's "not armed" (exit 1) withdraws consent.
for (const [label, code] of [['a timeout', 'throw'], ['another exit code', 2]] as const) {
  test(`a consent check that ends in ${label} keeps the stream and its lines (BUG-3741)`, async ($, on) => {
    const clock = mock.clock(on)
    const s = stubs(on, { armed: true, chunks: [{ stream: 'stdout', text: 'first\n' }] })
    await $.session.start({ cwd: '/work/demo' })
    await usePad($)
    await settle()
    s.consent.code = code
    await clock.advance(1500)
    await settle()
    expect(s.submitted).toEqual([])
    expect(s.stream.closed).toBe(false)
    s.consent.code = undefined
    s.stream.release()
    await settle()
    await clock.advance(1500)
    await settle()
    expect(s.submitted).toEqual(['first\nlate line'])
  })
}

test('a disarm that did not take effect (--help, a refusal) keeps the stream and flushes what waited (BUG-3741)', async ($, on) => {
  const clock = mock.clock(on)
  const s = stubs(on, { armed: true })
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await usePad($, 'pad session disarm --help')
  s.stream.release()
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.stream.closed).toBe(false)
  expect(s.submitted).toEqual(['late line'])
})

test('a consent check that answers armed while a disarm runs submits nothing (BUG-3741 r1)', async ($, on) => {
  const clock = mock.clock(on)
  const opts: any = { armed: true, holdSubmit: true, chunks: [{ stream: 'stdout', text: 'first\n' }] }
  const s = stubs(on, opts)
  await $.session.start({ cwd: '/work/demo' })
  await usePad($)
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  s.stream.release() // 'late line' waits behind the held submit
  await settle()
  await clock.advance(1500)
  await settle()
  // The disarm is running: the held submit returns and the flush asks
  // should-arm, which still answers armed, before the disarm takes effect.
  opts.onTool = async (e: any) => {
    if (!/disarm/.test(String(e.command))) return
    s.held.release()
    await settle()
    s.consent.armed = false
  }
  await usePad($, 'pad session disarm')
  await settle()
  s.stream.release() // an ended stream stops at its next write
  await settle()
  await clock.advance(1500)
  await settle()
  expect(s.submitted).toEqual(['first'])
  expect(s.stream.closed).toBe(true)
})
