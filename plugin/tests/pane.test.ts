import { expect, test } from 'claude-code/testing'

const PANE = {
  plugin: 'pad',
  component: 'Pane',
  requestId: 'pad',
  surface: 'terminal',
  viewport: { columns: 160, rows: 40 },
  props: { title: 'Pad', isFocused: true, bodyColumns: 70, placement: 'dock', scroll: { offset: 0, bodyRows: 30 }, view: {} },
} as const

const ITEM = {
  ref: 'TASK-7', title: 'Fix the login bug', collection_slug: 'tasks', collection_icon: '✅',
  // item show returns fields as a JSON STRING (regression: the pane printed it char by char)
  fields: JSON.stringify({ status: 'in-progress', priority: 'high' }), content: '## Plan\nDo the thing.',
  // only current decisions draw a chip; a superseded high answer must not
  decisions: [
    { question_set: 'conventions', question_key: 'conv:CONVE-3', current: true, answer: { type: 'noul', noul: 0.95 } },
    { question_set: 'conventions', question_key: 'conv:CONVE-4', current: false, answer: { type: 'noul', noul: 0.97 } },
  ],
}
const COLLECTIONS = [
  { slug: 'tasks', name: 'Tasks', icon: '✅', prefix: 'TASK', is_system: false, active_item_count: 2, item_count: 5,
    schema: JSON.stringify({ fields: [{ key: 'status', options: ['open', 'in-progress', 'done'] }] }) },
  { slug: 'conventions', name: 'Conventions', icon: '📏', prefix: 'CONVE', is_system: true, active_item_count: 9, schema: '{}' },
]
const DASHBOARD = { active_items: [{ item_ref: 'TASK-7', title: 'Fix the login bug', status: 'in-progress', collection_slug: 'tasks', updated_at: new Date().toISOString() }] }

// Answers for the pad CLI, keyed by its first words.
function cliAnswer(argv: readonly string[], calls: string[][]) {
  const a = argv.slice(1).filter((x) => x !== '--format' && x !== 'json')
  calls.push(a)
  const key = a.slice(0, 2).join(' ')
  const out: Record<string, unknown> = {
    'bootstrap': { workspace: { slug: 'demo', name: 'Demo' } },
    'project dashboard': DASHBOARD,
    'project ready': { count: 1, results: [{ item_ref: 'TASK-8', item_title: 'Write docs', collection: 'tasks', reason: 'high priority' }] },
    'project activity': [{ id: 'a1', action: 'commented', actor_name: 'Dave', metadata: '{"agent":"wren"}', item_ref: 'TASK-7', item_title: 'Fix the login bug', created_at: new Date().toISOString() }],
    'collection list': COLLECTIONS,
    'item show': ITEM,
    'item comments': [{ body: 'Started on it.', agent_name: 'wren', created_at: new Date().toISOString() }],
    'item list': [{ ref: 'TASK-7', title: 'Fix the login bug', fields: { status: 'in-progress' } }, { ref: 'TASK-9', title: 'Tidy', fields: { status: 'open' } }],
    'item search': { results: [{ item: { ref: 'TASK-7', title: 'Fix the login bug', fields: { status: 'in-progress' } }, rank: 1, snippet: '' }] },
    'item update': { ref: a[2] },
    'item comment': { ok: true },
    'item create': { ref: 'TASK-10', title: a[3] },
  }
  const k = a[0] === 'bootstrap' ? 'bootstrap' : key
  return { exitCode: 0, stdout: JSON.stringify(out[k] ?? {}), stderr: '' }
}

function stubs(on: any, opts: { cli?: boolean; mcp?: boolean; linked?: boolean; onFill?: (t: string) => void; claudeRuns?: string[][] } = {}) {
  const cli = opts.cli !== false
  const mcp = opts.mcp !== false
  const linked = opts.linked !== false
  const calls: string[][] = []
  const mcpCalls: { tool: string; args: Record<string, unknown> }[] = []
  const saved = new Map<string, unknown>()
  on('session.start', ($: any, e: any) => ({ cwd: e.cwd }))
  on('command.run', () => ({}))
  on('tool.call', () => ({ result: 'ok' }))
  on('session.cwd', () => ({ value: '/work/demo' }))
  on('fs.exists', ($: any, e: any) => ({ value: linked && e.path === '/work/demo/.pad.toml' }))
  on('fs.read', () => ({ value: 'workspace = "demo"\nurl = "https://pad.example"\n' }))
  on('store.get', ($: any, e: any) => ({ value: saved.get(e.key) }))
  on('store.set', ($: any, e: any) => { saved.set(e.key, e.value); return { value: undefined } })
  on('process.run', ($: any, e: any) => {
    if (e.argv[0] === 'claude') { opts.claudeRuns?.push(e.argv); return { value: { exitCode: 0, stdout: 'Added', stderr: '' } } }
    if (!cli) throw new Error('spawn pad ENOENT')
    return { value: cliAnswer(e.argv, calls) }
  })
  on('mcp.call', ($: any, e: any) => {
    mcpCalls.push({ tool: e.tool, args: e.args })
    if (!mcp || e.server !== 'pad') return { value: { isError: true, content: [{ type: 'text', text: 'no such server' }] } }
    const map: Record<string, unknown> = {
      'pad_collection:list': COLLECTIONS,
      'pad_project:dashboard': DASHBOARD,
      'pad_item:get': ITEM,
      'pad_item:list-comments': [],
      'pad_item:update': { ref: e.args.ref },
    }
    const body = map[e.tool + ':' + e.args.action] ?? {}
    return { value: { isError: false, content: [{ type: 'text', text: JSON.stringify(body) }] } }
  })
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.close', () => ({ value: undefined }))
  on('ui.toast', () => ({ value: undefined }))
  on('ui.copy', () => ({ value: undefined }))
  on('prompt.fill', ($: any, e: any) => { opts.onFill?.(e.text); return { isFilled: true } })
  on('command.register', () => ({ value: undefined }))
  on('clock.every', () => ({ value: undefined }))
  return { calls, mcpCalls, saved }
}

const settle = () => new Promise((r) => setTimeout(r, 30))

test('opens on Now, opens an item, changes its status and comments (CLI)', async ($, on) => {
  const { calls } = stubs(on)
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  expect(await ui.find({ key: 'row-now-TASK-7' })).toBeDefined()

  await ui.press({ key: 'row-now-TASK-7' })
  await settle()
  expect(await ui.find({ type: 'Text', text: 'Fix the login bug' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Started on it\./ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '◐ in-progress' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '! high' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /0: \{/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /Possibly breaks CONVE-3/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Possibly breaks CONVE-4/ })).toBeUndefined()

  await ui.press({ key: 'do-status' })
  await ui.select({ key: 'status-select', value: 'done' })
  await settle()
  expect(calls.some((c) => c.join(' ') === 'item update TASK-7 --status done')).toBe(true)

  await ui.press({ key: 'do-comment' })
  await ui.input({ key: 'comment-input', text: 'Looks good' })
  await settle()
  expect(calls.some((c) => c[0] === 'item' && c[1] === 'comment' && c[3] === 'Looks good')).toBe(true)

  await ui.press({ key: 'do-back' })
  expect(await ui.find({ key: 'row-now-TASK-7' })).toBeDefined()
  await ui.unmount()
})

test('Next, Browse, Find and Activity draw their rows', async ($, on) => {
  stubs(on)
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)

  await ui.press({ key: 'tab-next' })
  await settle()
  expect(await ui.find({ key: 'row-next-TASK-8' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /high priority/ })).toBeDefined()

  await ui.press({ key: 'tab-browse' })
  await settle()
  expect(await ui.find({ key: 'coll-tasks' })).toBeDefined()
  // system collections are listed too, after a Library heading; counts say open of total
  expect(await ui.find({ key: 'coll-conventions' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'Library' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '2 open · 5' })).toBeDefined()
  await ui.press({ key: 'coll-tasks' })
  await settle()
  expect(await ui.find({ key: 'row-browse-TASK-9' })).toBeDefined()

  await ui.press({ key: 'tab-find' })
  await ui.input({ key: 'find', text: 'login' })
  await settle()
  expect(await ui.find({ key: 'row-find-TASK-7' })).toBeDefined()

  await ui.press({ key: 'tab-activity' })
  await settle()
  expect(await ui.find({ key: 'act-a1' })).toBeDefined()
  await ui.unmount()
})

test('quick add creates an item and opens it', async ($, on) => {
  const { calls } = stubs(on)
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'add' })
  await settle()
  await ui.input({ key: 'add-title', text: 'New thing' })
  await settle()
  expect(calls.some((c) => c.join(' ') === 'item create tasks New thing')).toBe(true)
  await ui.unmount()
})

test('without the CLI it falls back to the Pad MCP server', async ($, on) => {
  const { mcpCalls } = stubs(on, { cli: false })
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  expect(await ui.find({ key: 'row-now-TASK-7' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'via mcp' })).toBeDefined()
  await ui.press({ key: 'row-now-TASK-7' })
  await settle()
  await ui.press({ key: 'do-status' })
  await ui.select({ key: 'status-select', value: 'done' })
  await settle()
  const upd = mcpCalls.find((c) => c.tool === 'pad_item' && c.args.action === 'update')
  expect(upd?.args).toMatchObject({ workspace: 'demo', ref: 'TASK-7', status: 'done' })
  await ui.unmount()
})

test('work on it fills the prompt instead of sending it', async ($, on) => {
  let filled = ''
  stubs(on, { onFill: (t) => { filled = t } })
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'row-now-TASK-7' })
  await settle()
  await ui.press({ key: 'do-work' })
  expect(filled).toBe("/pad let's work on TASK-7")
  await ui.unmount()
})

test('the Background setting is saved and paints the pane', async ($, on) => {
  const { saved } = stubs(on)
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  expect((await ui.find({ key: 'pane-root' }))?.props.backgroundColor).toBeUndefined()
  await ui.press({ key: 'tab-settings' })
  await ui.select({ key: 'set-background', value: 'darker' })
  await settle()
  expect((saved.get('settings') as any)?.background).toBe('darker')
  expect((await ui.find({ key: 'pane-root' }))?.props.backgroundColor).toBe('#1c1c1c')
  await ui.unmount()
})

test('on Desktop: native rules, no hint line, Settings on the top row, the ref in its own column', async ($, on) => {
  stubs(on)
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount({ ...PANE, surface: 'desktop' } as any)
  expect(await ui.find({ type: 'Text', text: /^─+$/ })).toBeUndefined()
  expect((await ui.findAll({ type: 'Svg' })).length).toBe(2) // one native rule under the header, one over the footer
  expect(await ui.find({ type: 'Text', text: /move · ⏎ open/ })).toBeUndefined()
  expect(await ui.find({ key: 'tab-settings' })).toBeDefined()
  expect((await ui.find({ key: 'row-now-TASK-7' }))?.props.label).toBe('Fix the login bug')
  expect(await ui.find({ type: 'Text', text: 'TASK-7' })).toBeDefined()
  await ui.unmount()
  // the terminal keeps all of it
  const term = await $.ui.mount(PANE)
  expect(await term.find({ type: 'Text', text: /^─+$/ })).toBeDefined()
  expect((await term.find({ key: 'row-now-TASK-7' }))?.props.label).toMatch(/^TASK-7\s+Fix the login bug/)
  await term.unmount()
})

test('with no CLI and no MCP server, the pane guides setup; Pad Cloud is added only on a second press', async ($, on) => {
  const filled: string[] = []
  const claudeRuns: string[][] = []
  stubs(on, { cli: false, mcp: false, linked: false, onFill: (t) => filled.push(t), claudeRuns })
  await $.session.start({ cwd: '/work/demo' })
  await $.command.run({ command: 'pad-pane', args: '' })
  await settle()
  const ui = await $.ui.mount(PANE)
  expect(await ui.find({ type: 'Text', text: 'Get started with Pad' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '○ pad CLI installed' })).toBeDefined()

  // Pad Cloud: the first press explains, the second adds it and fills /reload-plugins
  await ui.press({ key: 'gs-cloud' })
  await settle()
  expect(claudeRuns).toEqual([])
  expect(await ui.find({ type: 'Text', text: /Press 1 again/ })).toBeDefined()
  await ui.press({ key: 'gs-cloud' })
  await settle()
  expect(claudeRuns).toEqual([['claude', 'mcp', 'add', '--scope', 'user', '--transport', 'http', 'pad', 'https://mcp.getpad.dev']])
  expect(filled).toEqual(['/reload-plugins'])
  expect(await ui.find({ type: 'Text', text: /Authenticate/ })).toBeDefined()

  // this machine, without the CLI: Claude is asked to install it, never run unasked
  await ui.press({ key: 'gs-local' })
  await settle()
  expect(filled[1]).toMatch(/^Install the Pad CLI for me/)
  // own server
  await ui.press({ key: 'gs-server' })
  await settle()
  expect(filled[2]).toMatch(/self-hosted Pad server/)
  await ui.unmount()
})
