// The Pad pane inside Claude Code (IDEA-3477; folded into the pad plugin by TASK-3486).
//
// One command, /pad-pane, opens a pane on the linked workspace. Nothing else:
// no blocking, no band, no changes to what Claude sees. Until you run the
// command, the mod only listens for Claude touching Pad so an open pane stays
// fresh.
//
// Views: Now (n), Next (x), Browse (b), Find (f), Activity (a), Settings (g).
// Item view: status (s), comment (c), work on it (w), copy ref (y), open in
// link (o), back (h). Quick add: q. Refresh: r.

// ---------------------------------------------------------------- Pad client

// One Pad client, two transports. The pane only calls these methods, so it
// works the same with the `pad` CLI installed or with only an MCP connection.
//
// CLI: `pad … --format json`, which finds the workspace from .pad.toml and
//      carries its own credentials.
// MCP: $.mcp.call on a connected Pad server (a Pad plugin's server, the
//      remote mcp.getpad.dev, or a local `pad mcp`), using the workspace slug
//      from .pad.toml.

const MCP_SERVER_GUESSES = ['pad', 'claude.ai pad', 'plugin:pad:pad', 'getpad']

// .pad.toml from the session directory upwards: { workspace, url }.
async function readPadToml($) {
  let dir = String(await $.session.cwd())
  for (let i = 0; i < 40 && dir; i++) {
    const path = (dir === '/' ? '' : dir) + '/.pad.toml'
    try {
      if (await $.fs.exists(path)) {
        const text = String(await $.fs.read(path))
        const get = (k) => (text.match(new RegExp('^\\s*' + k + '\\s*=\\s*"([^"]*)"', 'm')) || [])[1]
        return { workspace: get('workspace'), url: get('url'), dir }
      }
    } catch {}
    if (dir === '/') break
    dir = dir.replace(/\/[^/]*$/, '') || '/'
  }
  return null
}

function parseMcp(result) {
  if (!result) throw new Error('no result')
  const text = (result.content || []).filter((b) => b.type === 'text').map((b) => b.text).join('')
  if (result.isError) throw new Error(text.split('\n')[0] || 'MCP error')
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

// Some endpoints return `fields` as a JSON string (item show), others as an
// object (lists). Always hand the pane an object.
function fieldsOf(f) {
  if (f && typeof f === 'object') return f
  if (typeof f === 'string') {
    try {
      const v = JSON.parse(f)
      return v && typeof v === 'object' ? v : {}
    } catch {}
  }
  return {}
}

const arr = (x, ...keys) => {
  if (Array.isArray(x)) return x
  for (const k of keys) if (x && Array.isArray(x[k])) return x[k]
  return []
}

function makeClient($, settings, toml) {
  let transport = null // 'cli' | 'mcp'
  let mcpServer = null
  let ws = toml?.workspace || null

  async function cli(args) {
    const r = await $.process.run(['pad', ...args, '--format', 'json'], { timeoutMs: 20000 })
    if (r.exitCode !== 0) throw new Error((r.stderr || r.stdout || 'pad failed').trim().split('\n')[0])
    return JSON.parse(r.stdout)
  }

  async function mcp(tool, args) {
    return parseMcp(await $.mcp.call(mcpServer, tool, { workspace: ws, ...args }))
  }

  async function connect() {
    const want = settings.transport // 'auto' | 'cli' | 'mcp'
    if (want !== 'mcp') {
      try {
        const boot = await cli(['bootstrap'])
        if (boot?.workspace?.slug) {
          ws = boot.workspace.slug
          transport = 'cli'
          return { transport, ws }
        }
      } catch (err) {
        if (want === 'cli') throw err
      }
    }
    const names = settings.mcpServer ? [settings.mcpServer] : MCP_SERVER_GUESSES
    for (const name of names) {
      try {
        mcpServer = name
        await mcp('pad_collection', { action: 'list' })
        transport = 'mcp'
        return { transport, ws, server: name }
      } catch {}
    }
    mcpServer = null
    throw new Error(
      ws
        ? 'No way to reach Pad: the pad CLI is not installed or not linked here, and no Pad MCP server answered.'
        : 'This folder is not linked to a Pad workspace (no .pad.toml).'
    )
  }

  const via = (cliFn, mcpFn) => (...a) => (transport === 'cli' ? cliFn(...a) : mcpFn(...a))

  return {
    connect,
    get transport() { return transport },
    get workspace() { return ws },
    webUrl(ref) {
      const base = (toml?.url || 'https://app.getpad.dev').replace(/\/$/, '')
      return `${base}/-/r/${ws}/${ref}`
    },
    dashboard: via(() => cli(['project', 'dashboard']), () => mcp('pad_project', { action: 'dashboard' })),
    ready: via(() => cli(['project', 'ready']), () => mcp('pad_project', { action: 'ready' })),
    activity: via(
      (limit) => cli(['project', 'activity', '--limit', String(limit)]),
      (limit) => mcp('pad_project', { action: 'activity', limit })
    ),
    collections: via(() => cli(['collection', 'list']), () => mcp('pad_collection', { action: 'list' })),
    list: via(
      (collection) => cli(['item', 'list', collection, '--limit', '100']),
      (collection) => mcp('pad_item', { action: 'list', collection, limit: 100 })
    ),
    get: via((ref) => cli(['item', 'show', ref]), (ref) => mcp('pad_item', { action: 'get', ref })),
    comments: via(
      (ref) => cli(['item', 'comments', ref]),
      (ref) => mcp('pad_item', { action: 'list-comments', ref })
    ),
    search: via(
      (q) => cli(['item', 'search', q, '--limit', '30']),
      (q) => mcp('pad_search', { action: 'query', query: q, limit: 30 })
    ),
    setStatus: via(
      (ref, status) => cli(['item', 'update', ref, '--status', status]),
      (ref, status) => mcp('pad_item', { action: 'update', ref, status })
    ),
    comment: via(
      (ref, text) => cli(['item', 'comment', ref, text]),
      (ref, text) => mcp('pad_item', { action: 'comment', ref, message: text })
    ),
    create: via(
      (collection, title) => cli(['item', 'create', collection, title]),
      (collection, title) => mcp('pad_item', { action: 'create', collection, title })
    ),
  }
}

// Shapes differ a little between endpoints and transports; these flatten them.
const norm = {
  row(x) {
    return {
      ref: x.ref || x.item_ref || x.item?.ref,
      title: x.title || x.item_title || x.item?.title || '',
      status: x.status || fieldsOf(x.fields).status || fieldsOf(x.item?.fields).status || '',
      // Bugs rank by severity; show it where a priority would go.
      priority: x.priority || fieldsOf(x.fields).priority || fieldsOf(x.item?.fields).priority || x.severity || fieldsOf(x.fields).severity || '',
      collection: x.collection_slug || x.collection || x.item?.collection_slug || '',
      icon: x.collection_icon || x.item?.collection_icon || '',
      updated: x.updated_at || x.item?.updated_at || '',
      reason: x.reason || '',
    }
  },
  activeItems: (d) => arr(d?.active_items).map(norm.row),
  ready: (r) => arr(r, 'results').map(norm.row),
  list: (r) => arr(r, 'items', 'results').map(norm.row),
  search: (r) => arr(r, 'results').map((x) => norm.row(x.item ? { ...x.item, ...x } : x)),
  comments: (r) => arr(r, 'comments'),
  activity: (r) => arr(r, 'activities', 'results'),
  collections(r) {
    return arr(r, 'collections').map((c) => {
      let schema = c.schema
      if (typeof schema === 'string') {
        try { schema = JSON.parse(schema) } catch { schema = {} }
      }
      const status = (schema?.fields || []).find((f) => f.key === 'status')
      return {
        slug: c.slug, name: c.name, icon: c.icon || '·', prefix: c.prefix,
        system: !!c.is_system, open: c.active_item_count ?? null, total: c.item_count ?? 0,
        statuses: status?.options || [], terminal: status?.terminal_options || [],
      }
    })
  },
}

function ago(iso) {
  if (!iso) return ''
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000)
  if (s < 60) return Math.floor(s) + 's'
  if (s < 3600) return Math.floor(s / 60) + 'm'
  if (s < 86400) return Math.floor(s / 3600) + 'h'
  return Math.floor(s / 86400) + 'd'
}

// ---------------------------------------------------------------- pane state

const PANE = 'pad'
const PLUGIN = 'pad' // as plugin.json names it: ui.focus redirects carry it
const DEFAULTS = { startView: 'now', refreshSec: 60, transport: 'auto', mcpServer: '', background: 'theme' }
// The engine fills a pane with the terminal's "bright black", whose shade is the
// terminal palette's; these paint the pane's body over it. 'theme' leaves it be.
const BACKGROUNDS = { theme: undefined, dark: '#262626', darker: '#1c1c1c', black: '#121212' }

let settings = { ...DEFAULTS }
let client = null
let conn = null // { transport, ws } once connected
let connError = null
let open = false

let view = 'now'
const stack = []
const data = { now: [], next: [], activity: [], collections: [], list: [], results: [], query: '' }
let browseCollection = null
let item = null // { ref, raw, comments, statusOptions }
let mode = null // null | 'status' | 'comment' | 'add'
let addCollection = null
let loadedAt = null
let busy = false
let notice = null

// Keyboard model. Tab and Shift+Tab walk the menu: the view tabs, the list as ONE
// stop, then the view's other controls. Up and Down walk the list's rows.
// The engine reports Tab only as a focus move and an arrow only as a one-row
// scroll whenever the drawing is taller than the pane, so the drawing is always
// kept one row taller, and the two hooks below give each key its own meaning.
const ring = { order: [], cur: null, lastRow: {}, terminal: true }
const LIST_KEY = /^(row-|coll-|act-|empty-back$)/
const NOT_A_STOP = /^(refresh|add)$/ // footer actions: their hotkeys reach them
const stopOf = (k) => (LIST_KEY.test(k) ? 'LIST' : k)
const listKeys = () => ring.order.filter((k) => LIST_KEY.test(k))

// Where a person's focus move should land, or null to let it land where the
// engine aimed it. `order` is the drawn controls; `to` is the engine's target
// (undefined for one of its own stops, such as the close mark). A move of one
// step either way is Tab or Shift+Tab; anything else is a click.
// Returns { element, engineStop } (engineStop: the engine aimed at its own stop).
export function tabStep(order, cur, to, listEntry) {
  const n = order.length
  const dT = order.indexOf(to), dC = order.indexOf(cur)
  if (n < 2 || (dT < 0 && dC < 0)) return null
  const stops = []
  for (const k of order) {
    if (NOT_A_STOP.test(k)) continue
    const s = stopOf(k)
    if (stops[stops.length - 1] !== s) stops.push(s)
  }
  const land = (target, engineStop = false) => ({ element: target === 'LIST' ? listEntry : target, engineStop })
  const at = stops.indexOf(stopOf(cur))
  if (dT >= 0 && dC >= 0) {
    const dir = (dT - dC + n) % n === 1 ? 1 : (dC - dT + n) % n === 1 ? -1 : 0
    if (!dir) return null
    return land(stops[(at + dir + stops.length) % stops.length])
  }
  if (dC >= 0) {
    if (at === 0) return land(stops[stops.length - 1], true)
    if (at === stops.length - 1) return land(stops[0], true)
    return null
  }
  // coming back from one of the engine's stops: enter at whichever end is nearer
  return land(dT > n / 2 ? stops[stops.length - 1] : stops[0])
}

// The row an arrow (a one-row scroll) moves to, or null to let it scroll.
export function arrowStep(order, cur, by, listEntry) {
  if (Math.abs(by) !== 1) return null
  const rows = order.filter((k) => LIST_KEY.test(k))
  if (!rows.length) return null
  const i = rows.indexOf(cur)
  if (i < 0) return by > 0 ? listEntry : null
  return rows[i + by] || null
}
const firstRowFor = () => {
  const rows = listKeys()
  const last = ring.lastRow[view + ':' + (browseCollection || '')]
  return rows.includes(last) ? last : rows[0]
}

const redraw = ($) => $.ui.invalidate('ui.render')
const msg = (err) => String(err?.message || err).slice(0, 120)

async function ensureClient($) {
  if (client && conn) return true
  const toml = await readPadToml($)
  client = makeClient($, settings, toml)
  try {
    conn = await client.connect()
    connError = null
    return true
  } catch (err) {
    conn = null
    connError = msg(err)
    return false
  }
}

async function ensureCollections() {
  if (!data.collections.length) data.collections = norm.collections(await client.collections())
}

async function load($, what = view) {
  if (!(await ensureClient($))) return redraw($)
  busy = true
  redraw($)
  try {
    if (what === 'now') data.now = norm.activeItems(await client.dashboard())
    else if (what === 'next') data.next = norm.ready(await client.ready())
    else if (what === 'activity') data.activity = norm.activity(await client.activity(30))
    else if (what === 'browse') {
      data.collections = norm.collections(await client.collections())
      if (browseCollection) data.list = norm.list(await client.list(browseCollection))
    } else if (what === 'item' && item) await fetchItem($, item.ref)
    loadedAt = new Date().toISOString()
    notice = null
  } catch (err) {
    notice = 'Pad: ' + msg(err)
  }
  busy = false
  redraw($)
}

async function fetchItem($, ref) {
  const got = await client.get(ref)
  const raw = { ...got, fields: fieldsOf(got?.fields) }
  const comments = norm.comments(await client.comments(ref))
  await ensureCollections()
  const coll = data.collections.find((c) => c.slug === raw.collection_slug)
  item = { ref, raw, comments, statusOptions: coll?.statuses || [] }
  loadedAt = new Date().toISOString()
}

async function openItem($, ref) {
  if (!(await ensureClient($))) return redraw($)
  if (view !== 'item') stack.push(view)
  view = 'item'
  mode = null
  busy = true
  redraw($)
  try {
    await fetchItem($, ref)
    notice = null
  } catch (err) {
    notice = 'Pad: could not open ' + ref + ': ' + msg(err)
  }
  busy = false
  redraw($)
}

function back($) {
  mode = null
  view = stack.pop() || settings.startView
  redraw($)
}

// One level up from where the pane is, or false at the top level.
function stepBack($) {
  if (mode) {
    mode = null
  } else if (view === 'item') {
    view = stack.pop() || settings.startView
  } else if (view === 'browse' && browseCollection) {
    browseCollection = null
  } else if (view !== settings.startView) {
    view = settings.startView
    if (view !== 'find' && view !== 'settings') load($, view)
  } else {
    return false
  }
  redraw($)
  return true
}

const OPEN_ARGS = { id: PANE, title: 'Pad', focus: true, closeOnEscape: true }

function go($, v) {
  mode = null
  stack.length = 0
  view = v
  if (v === 'browse') browseCollection = null
  if (v === 'find' || v === 'settings') return redraw($)
  load($, v)
}

// ---------------------------------------------------------------- drawing

// ---------------------------------------------------------------- look

// Colours are theme keys, so the pane follows the user's light or dark theme.
const C = { brand: 'merged', open: 'suggestion', active: 'warning', done: 'success', blocked: 'error', muted: 'inactive', accent: 'claude' }
const DONE = /^(done|fixed|completed?|implemented|published|merged|resolved|shipped|archived|consumed)$/
const ACTIVE = /^(in[-_ ]?progress|doing|fixing|exploring|implementing|in[-_ ]?review|review|testing|active|drafting|writing|running)$/
const BLOCKED = /^(blocked|rejected|wont[-_ ]?fix|cancell?ed|failed|deprecated)$/

function statusColor(s) {
  s = String(s || '').toLowerCase()
  if (DONE.test(s)) return C.done
  if (ACTIVE.test(s)) return C.active
  if (BLOCKED.test(s)) return C.blocked
  return s ? C.open : C.muted
}
const statusGlyph = (s) => {
  s = String(s || '').toLowerCase()
  if (DONE.test(s)) return '✔'
  if (ACTIVE.test(s)) return '◐'
  if (BLOCKED.test(s)) return '✖'
  return s ? '○' : '·'
}
function priorityMark(p) {
  p = String(p || '').toLowerCase()
  if (p === 'critical' || p === 'urgent') return { t: '‼', c: C.blocked }
  if (p === 'high' || p === 'must') return { t: '!', c: C.blocked }
  if (p === 'low' || p === 'may') return { t: '↓', c: C.muted }
  return { t: ' ', c: C.muted }
}

const HINTS = {
  now: '↑↓ move · ⏎ open · q add · r refresh · esc close',
  next: '↑↓ move · ⏎ open · q add · r refresh · esc back',
  browse: '↑↓ move · ⏎ open · esc back · q add · r refresh',
  find: 'type, then ⏎ to search · ↑↓ results · esc back',
  activity: '↑↓ move · ⏎ open the item · r refresh · esc back',
  settings: '↑↓ move · ⏎ change · esc back',
  item: 's status · c comment · w work on it · y copy ref · o copy link · esc back',
}

function draw($, e) {
  const ui = $.ui.resolve(e)
  const { Text, Markdown } = ui
  ring.order = []
  let autoKey = null
  const stop = (make) => (props) => {
    if (props?.key) { ring.order.push(props.key); if (props.autoFocus && !autoKey) autoKey = props.key }
    return make(props)
  }
  const Button = stop(ui.Button), Input = stop(ui.Input), Select = stop(ui.Select)
  // Keep every box at its natural height: when the drawing is taller than the pane
  // it scrolls as a whole, but flex would first squeeze bordered boxes to nothing.
  const Box = (props) => ui.Box({ flexShrink: 0, ...props })
  const W = Math.max(28, (e.props.bodyColumns || 60) - 1)
  // The Desktop app draws these elements natively, in a proportional font: no
  // character-drawn rules or space-padded columns there, and its keycap badges
  // already show each hotkey.
  const desktop = e.surface === 'desktop'
  // the keyboard model is the terminal's: Desktop's native focus needs none of it,
  // and there a click one control away read as Tab and was redirected
  ring.terminal = !desktop
  const clip = (t, n = W) => {
    t = String(t ?? '')
    return t.length > n ? t.slice(0, Math.max(1, n - 1)) + '…' : t
  }
  const T = (children, props = {}) => Text({ ...props, children: Array.isArray(children) ? children : [children] })
  const dim = (t) => T(t, { dimColor: true })
  const gap = () => T(' ')
  // Desktop draws a run of '─' as two lines; its own vector leaf draws exactly one.
  // This markup is the one that showed (2026-10-08): solid #888, 2px. A 1px,
  // translucent, 2400-wide version drew nothing.
  // The line sits at the top of an 8px leaf, so 6px of space follows it.
  const RULE_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="600" height="8" viewBox="0 0 600 8" preserveAspectRatio="none"><rect x="0" y="0" width="600" height="2" fill="#888888"/></svg>'
  const rule = () =>
    desktop
      ? (ui.Svg ? ui.Svg({ source: RULE_SVG, alt: 'divider', height: 8 }) : null)
      : T('─'.repeat(W), { color: C.muted })
  const section = (label, extra) =>
    Box({ flexDirection: 'row', columnGap: 1, children: [T(label, { bold: true, color: C.brand }), ...(extra ? [dim(extra)] : [])] })

  // ---- header: brand, workspace, tabs
  const tab = (v, label, hotkey) =>
    Button({ key: 'tab-' + v, label, hotkey, plain: true, dimColor: view !== v && !(view === 'item' && stack[0] === v), onPress: () => go($, v) })
  const header = Box({
    flexDirection: 'column',
    children: [
      Box({
        flexDirection: 'row',
        justifyContent: 'space-between',
        children: [
          Box({ flexDirection: 'row', columnGap: 1, children: [T('▣ pad', { bold: true, color: C.brand }), T(conn ? conn.ws : '…', { bold: true })] }),
          Box({
            flexDirection: 'row',
            columnGap: 2,
            children: [
              dim(busy ? 'loading…' : loadedAt ? 'updated ' + ago(loadedAt) + ' ago' : ''),
              ...(desktop ? [tab('settings', 'Settings', 'g')] : []),
            ],
          }),
        ],
      }),
      Box({
        flexDirection: 'row',
        columnGap: desktop ? 1 : 2,
        flexWrap: 'wrap',
        children: [
          tab('now', 'Now', 'n'),
          tab('next', 'Next', 'x'),
          tab('browse', 'Browse', 'b'),
          tab('find', 'Find', 'f'),
          tab('activity', 'Activity', 'a'),
          ...(desktop ? [] : [tab('settings', 'Settings', 'g')]),
        ],
      }),
      rule(),
    ].filter(Boolean),
  })

  // ---- one item row: glyph, priority, ref (fixed width), title, status, age
  const REFW = 10
  const row = (r, i, opts = {}) => {
    const sc = statusColor(r.status)
    const pm = priorityMark(r.priority)
    const tail = [opts.hideStatus ? '' : r.status, r.updated ? ago(r.updated) : ''].filter(Boolean).join(' · ')
    const titleW = Math.max(8, W - REFW - tail.length - 7)
    return Box({
      key: 'r-' + view + '-' + (r.ref || i),
      flexDirection: 'row',
      columnGap: 1,
      children: [
        T(statusGlyph(r.status), { color: sc }),
        T(pm.t, { color: pm.c }),
        // Desktop's font is proportional, so spaces can't hold the ref column:
        // give the ref a fixed-width box and let the Button carry the title
        ...(desktop ? [Box({ width: REFW, flexShrink: 0, children: [T(r.ref || '', { dimColor: true })] })] : []),
        Button({
          key: 'row-' + view + '-' + (r.ref || i),
          plain: true,
          ...(i === 0 && !opts.noFocus ? { autoFocus: true } : {}),
          label: desktop ? clip(r.title, titleW) : (r.ref || '').padEnd(REFW) + ' ' + clip(r.title, titleW),
          onPress: () => openItem($, r.ref),
        }),
        Box({ flexGrow: 1 }),
        T(tail, { color: opts.hideStatus ? C.muted : sc, dimColor: opts.hideStatus }),
      ],
    })
  }
  const empty = (t) => [dim(busy ? '  loading…' : '  ' + t)]

  let body = []
  if (!conn) {
    body = [
      T('Not connected to Pad', { bold: true, color: C.blocked }),
      T(clip(connError || 'Connecting…', W * 3)),
      gap(),
      dim('Link this folder with `pad init`, or connect the Pad MCP server with /mcp, then press r.'),
    ]
  } else if (mode === 'add') {
    const colls = data.collections.filter((c) => !c.system)
    body = [
      section('New item'),
      gap(),
      Select({
        key: 'add-collection',
        label: 'Collection',
        value: addCollection || colls[0]?.slug,
        options: colls.map((c) => ({ value: c.slug, label: `${c.icon} ${c.name}` })),
        onSelect: (v) => {
          addCollection = v
          redraw($)
        },
      }),
      Input({
        key: 'add-title',
        label: 'Title',
        placeholder: 'What needs doing? ⏎ to create',
        autoFocus: true,
        submitLabel: 'Create',
        onSubmit: async (title) => {
          if (!String(title).trim()) return
          try {
            const created = await client.create(addCollection || colls[0]?.slug, String(title).trim())
            const ref = created?.ref || created?.item?.ref
            mode = null
            $.ui.toast('Created ' + (ref || 'item'))
            if (ref) await openItem($, ref)
            else redraw($)
          } catch (err) {
            notice = 'Create failed: ' + msg(err)
            redraw($)
          }
        },
      }),
      gap(),
      Button({ key: 'add-cancel', label: 'cancel', hotkey: 'h', plain: true, dimColor: true, onPress: () => { mode = null; redraw($) } }),
    ]
  } else if (view === 'now') {
    body = [section('In progress', data.now.length ? String(data.now.length) : ''), ...(data.now.length ? data.now.map((r, i) => row(r, i)) : empty('Nothing in progress.'))]
  } else if (view === 'next') {
    body = [section('Ready to work on', data.next.length ? String(data.next.length) : '')]
    data.next.forEach((r, i) => {
      body.push(row(r, i))
      if (r.reason) body.push(T('     ' + clip(r.reason, W - 5), { color: C.muted, italic: true }))
    })
    if (!data.next.length) body.push(...empty('Nothing ready.'))
  } else if (view === 'browse') {
    if (!browseCollection) {
      body = [section('Collections')]
      const user = data.collections.filter((c) => !c.system)
      const lib = data.collections.filter((c) => c.system)
      for (const c of [...user, ...(lib.length ? [{ heading: 'Library' }] : []), ...lib]) {
        if (c.heading) { body.push(T(c.heading, { key: 'ch-lib', bold: true, color: C.muted })); continue }
        body.push(
          Box({
            key: 'cb-' + c.slug,
            flexDirection: 'row',
            columnGap: 1,
            children: [
              Button({ key: 'coll-' + c.slug, plain: true, label: `${c.icon} ${clip(c.name, W - 12)}`, onPress: async () => {
                browseCollection = c.slug; data.list = []
                await load($, 'browse')
                // An empty collection's only other stop is the header's back Button;
                // put the ring on the vertical back row instead (the engine keeps a
                // ring the person moved, so autoFocus alone doesn't move it).
                if (!data.list.length) $.ui.focus({ requestId: PANE, key: 'empty-back' }).catch(() => {})
              } }),
              Box({ flexGrow: 1 }),
              dim(c.open == null || c.open === c.total ? String(c.total) : `${c.open} open · ${c.total}`),
            ],
          })
        )
      }
      if (body.length === 1) body.push(...empty('No collections.'))
    } else {
      const coll = data.collections.find((c) => c.slug === browseCollection)
      body = [
        Box({
          flexDirection: 'row',
          columnGap: 1,
          children: [Button({ key: 'browse-up', label: '‹ Collections', hotkey: 'h', plain: true, dimColor: true, onPress: () => { browseCollection = null; redraw($) } }), T('/', { color: C.muted }), T(`${coll?.icon || ''} ${coll?.name || browseCollection}`, { bold: true, color: C.brand })],
        }),
      ]
      const groups = new Map()
      for (const r of data.list) {
        if (!groups.has(r.status)) groups.set(r.status, [])
        groups.get(r.status).push(r)
      }
      const order = coll?.statuses || []
      const keys = [...groups.keys()].sort((a, b) => (order.indexOf(a) + 1 || 99) - (order.indexOf(b) + 1 || 99))
      for (const status of keys) {
        const rows = groups.get(status)
        body.push(gap(), Box({ flexDirection: 'row', columnGap: 1, children: [T(statusGlyph(status), { color: statusColor(status) }), T(status || 'no status', { bold: true, color: statusColor(status) }), dim(String(rows.length))] }))
        rows.forEach((r, i) => body.push(row(r, i, { hideStatus: true, noFocus: status !== keys[0] })))
      }
      if (!data.list.length) {
        // An empty collection still gives the ring a vertical stop, or it falls
        // back onto the header's back Button, where ← looks like it should work.
        body.push(...empty('No open items.'), Button({ key: 'empty-back', label: '‹ Back to collections', plain: true, autoFocus: true, onPress: () => { browseCollection = null; redraw($) } }))
      }
    }
  } else if (view === 'find') {
    body = [
      Input({
        key: 'find',
        label: 'Search',
        placeholder: 'Words, then ⏎',
        value: data.query,
        autoFocus: true,
        onSubmit: async (q) => {
          data.query = String(q)
          if (!data.query.trim() || !(await ensureClient($))) return
          busy = true
          redraw($)
          try {
            data.results = norm.search(await client.search(data.query))
            notice = null
          } catch (err) {
            notice = 'Search failed: ' + msg(err)
          }
          busy = false
          redraw($)
        },
      }),
    ]
    if (data.results.length) body.push(gap(), section('Results for “' + clip(data.query, W - 24) + '”', String(data.results.length)), ...data.results.map((r, i) => row(r, i)))
    else if (data.query && !busy) body.push(gap(), dim('  No matches for “' + data.query + '”.'))
  } else if (view === 'activity') {
    body = [section('Recent activity')]
    data.activity.forEach((a, i) => {
      let who = a.actor_name || a.user_name || 'someone'
      let isAgent = a.actor === 'agent'
      try {
        const m = typeof a.metadata === 'string' ? JSON.parse(a.metadata) : a.metadata
        if (m?.agent) { who = m.agent; isAgent = true }
      } catch {}
      const what = String(a.action || '').replace(/_/g, ' ')
      body.push(
        Box({
          key: 'ab-' + (a.id || i),
          flexDirection: 'row',
          columnGap: 1,
          children: [
            T(ago(a.created_at).padStart(3), { color: C.muted }),
            T(clip(who, 12).padEnd(12), { color: isAgent ? C.accent : C.open, bold: true }),
            T(clip(what, 12).padEnd(12), { dimColor: true }),
            Button({ key: 'act-' + (a.id || i), plain: true, label: clip(`${a.item_ref || ''}  ${a.item_title || ''}`, Math.max(10, W - 32)), onPress: () => a.item_ref && openItem($, a.item_ref) }),
          ],
        })
      )
    })
    if (!data.activity.length) body.push(...empty('No recent activity.'))
  } else if (view === 'settings') {
    const set = async (k, v) => {
      settings = { ...settings, [k]: v }
      await $.store.set('settings', settings)
      if (k === 'transport') {
        client = null
        conn = null
        await ensureClient($)
      }
      redraw($)
    }
    body = [
      section('Settings'),
      gap(),
      Select({ key: 'set-start', label: 'Opens on', value: settings.startView, options: ['now', 'next', 'browse', 'activity'].map((v) => ({ value: v, label: v })), onSelect: (v) => set('startView', v) }),
      Select({ key: 'set-refresh', label: 'Refresh while open', value: String(settings.refreshSec), options: [['30', 'every 30s'], ['60', 'every minute'], ['300', 'every 5 min'], ['0', 'only when I press r']].map(([value, label]) => ({ value, label })), onSelect: (v) => set('refreshSec', Number(v)) }),
      Select({ key: 'set-background', label: 'Background', value: settings.background, options: [['theme', 'the terminal\'s pane colour'], ['dark', 'dark grey'], ['darker', 'darker grey'], ['black', 'near black']].map(([value, label]) => ({ value, label })), onSelect: (v) => set('background', v) }),
      Select({ key: 'set-transport', label: 'Talk to Pad through', value: settings.transport, options: [['auto', 'CLI if installed, else MCP'], ['cli', 'the pad CLI'], ['mcp', 'the Pad MCP server']].map(([value, label]) => ({ value, label })), onSelect: (v) => set('transport', v) }),
      gap(),
      T(conn ? `● connected to ${conn.ws} via ${conn.transport}${conn.server ? ' (' + conn.server + ')' : ''}` : '○ not connected', { color: conn ? C.done : C.blocked }),
    ]
  } else if (view === 'item' && item) {
    const f = item.raw.fields || {}
    const sc = statusColor(f.status)
    const pm = priorityMark(f.priority || f.severity)
    const meta = Object.entries(f)
      .filter(([k, v]) => !['status', 'priority', 'severity'].includes(k) && v !== null && v !== '' && typeof v !== 'object')
      .map(([k, v]) => `${k}: ${v}`)
    const act = (key, label, hotkey, onPress) => Button({ key: 'do-' + key, label, hotkey, plain: true, onPress })
    const card = Box({
      flexDirection: 'column',
      // 'quote', not 'round': in a pane, a full border drew empty (CC 2.1.293).
      borderStyle: 'quote',
      borderColor: sc,
      paddingLeft: 1,
      paddingRight: 1,
      children: [
        Box({
          flexDirection: 'row',
          justifyContent: 'space-between',
          children: [T(item.ref, { bold: true, color: C.brand }), T(item.raw.updated_at ? 'updated ' + ago(item.raw.updated_at) + ' ago' : '', { dimColor: true })],
        }),
        T(item.raw.title || '', { bold: true, wrap: 'wrap' }),
        Box({
          flexDirection: 'row',
          columnGap: 2,
          flexWrap: 'wrap',
          children: [
            T(`${statusGlyph(f.status)} ${f.status || 'no status'}`, { color: sc, bold: true }),
            ...(f.priority || f.severity ? [T(`${pm.t.trim() || '·'} ${f.priority || f.severity}`, { color: pm.c })] : []),
            ...(item.raw.parent_ref ? [dim('parent ' + item.raw.parent_ref)] : []),
          ],
        }),
        ...(meta.length ? [dim(clip(meta.join('  ·  '), W * 2))] : []),
        ...(item.raw.decisions || [])
          .filter((d) => d.question_set === 'conventions' && d.current !== false && d.answer?.noul >= 0.9)
          .map((d) => T('⚑ Possibly breaks ' + String(d.question_key).replace(/^conv:/, ''), { color: C.active })),
      ],
    })
    body = [
      card,
      Box({
        flexDirection: 'row',
        columnGap: 2,
        flexWrap: 'wrap',
        children: [
          act('status', 'status', 's', () => { mode = mode === 'status' ? null : 'status'; redraw($) }),
          act('comment', 'comment', 'c', () => { mode = mode === 'comment' ? null : 'comment'; redraw($) }),
          act('work', 'work on it', 'w', async () => {
            await $.prompt.fill({ text: `/pad let's work on ${item.ref}` })
            $.ui.toast('In your prompt: edit it or press Enter')
          }),
          act('copy', 'copy ref', 'y', (press) => { $.ui.copy({ text: item.ref, surface: press?.surface }); $.ui.toast('Copied ' + item.ref) }),
          act('open', 'copy link', 'o', (press) => { const url = client.webUrl(item.ref); $.ui.copy({ text: url, surface: press?.surface }); $.ui.toast('Link copied') }),
          act('back', 'back', 'h', () => back($)),
        ],
      }),
    ]
    if (mode === 'status') {
      body.push(
        Select({
          key: 'status-select',
          label: 'Status',
          value: f.status,
          autoFocus: true,
          options: (item.statusOptions.length ? item.statusOptions : [f.status || 'open']).map((s) => ({ value: s, label: `${statusGlyph(s)} ${s}` })),
          onSelect: async (s) => {
            if (s === f.status) { mode = null; return redraw($) }
            try {
              await client.setStatus(item.ref, s)
              $.ui.toast(`${item.ref} → ${s}`)
              mode = null
              await fetchItem($, item.ref)
            } catch (err) {
              notice = 'Status change failed: ' + msg(err)
            }
            redraw($)
          },
        })
      )
    }
    if (mode === 'comment') {
      body.push(
        Input({
          key: 'comment-input',
          label: 'Comment',
          placeholder: '⏎ to post',
          autoFocus: true,
          submitLabel: 'Post',
          onSubmit: async (text) => {
            if (!String(text).trim()) return
            try {
              await client.comment(item.ref, String(text).trim())
              $.ui.toast('Commented on ' + item.ref)
              mode = null
              await fetchItem($, item.ref)
            } catch (err) {
              notice = 'Comment failed: ' + msg(err)
            }
            redraw($)
          },
        })
      )
    }
    const content = String(item.raw.content || '').trim()
    body.push(gap())
    if (content) body.push(Markdown({ key: 'body-' + item.ref, text: content.length > 6000 ? content.slice(0, 6000) + '\n\n…' : content }))
    else body.push(dim('No description.'))
    if (item.comments.length) {
      body.push(gap(), section('Comments', String(item.comments.length) + (item.comments.length > 5 ? ' · latest 5' : '')))
      for (const c of item.comments.slice(-5)) {
        const who = c.agent_name || c.author || '?'
        body.push(
          Box({
            flexDirection: 'column',
            // 'quote', not 'round': in a pane, a full border drew empty (CC 2.1.293).
      borderStyle: 'quote',
            borderColor: c.agent_name ? C.accent : C.open,
            paddingLeft: 1,
            marginTop: 1,
            children: [
              Box({ flexDirection: 'row', columnGap: 1, children: [T(who, { bold: true, color: c.agent_name ? C.accent : C.open }), dim(ago(c.created_at) + ' ago')] }),
              T(clip(String(c.body || '').replace(/\s+/g, ' ').trim(), W * 4), { wrap: 'wrap' }),
            ],
          })
        )
      }
    }
  }

  const footer = Box({
    flexDirection: 'column',
    children: [
      rule(),
      Box({
        flexDirection: 'row',
        columnGap: 2,
        children: [
          Button({ key: 'refresh', label: 'refresh', hotkey: 'r', plain: true, dimColor: true, onPress: () => load($) }),
          Button({
            key: 'add',
            label: 'quick add',
            hotkey: 'q',
            plain: true,
            dimColor: true,
            onPress: async () => {
              if (!(await ensureClient($))) return redraw($)
              await ensureCollections()
              mode = 'add'
              redraw($)
            },
          }),
          Box({ flexGrow: 1 }),
          ...(conn ? [dim('via ' + conn.transport)] : []),
        ],
      }),
      ...(desktop
        ? []
        : [
            e.props.isFocused
              ? dim(clip(HINTS[mode === 'add' ? 'find' : view] || '', W))
              : T(clip('ctrl+x tab: use the pane · ctrl+x x: close', W), { color: C.accent }),
          ]),
    ].filter(Boolean),
  })

  // A new view drops the element the ring was on; the engine then shows it on
  // the view's autoFocus control without raising ui.focus, so follow it here.
  if (!ring.order.includes(ring.cur)) ring.cur = autoKey || ring.order.find((k) => LIST_KEY.test(k)) || null
  const bg = BACKGROUNDS[settings.background]
  return Box({
    key: 'pane-root',
    flexDirection: 'column',
    // one row taller than the pane, always: see the keyboard model at the top
    ...(desktop ? {} : { minHeight: (e.props.scroll?.bodyRows || 0) + 1 }),
    ...(bg ? { backgroundColor: bg } : {}),
    children: [header, ...(notice ? [T('⚠ ' + clip(notice, W * 2), { color: C.blocked })] : []), ...body, gap(), footer],
  })
}

// ---------------------------------------------------------------- hooks

export function register(on) {
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'pad-pane', description: 'Open or close the Pad pane for this workspace', immediate: true })
    const saved = await $.store.get('settings')
    if (saved && typeof saved === 'object') settings = { ...DEFAULTS, ...saved }
    // After a reload (an update, or a --plugin-dir edit) the pane may still be
    // open while this module starts empty: pick it back up.
    try {
      const panes = await $.ui.panes()
      if (panes.some((p) => p.id === PANE)) {
        open = true
        load($)
      }
    } catch {}
    $.clock.every(5000, () => {
      if (!open || !settings.refreshSec || busy || mode || view === 'find' || view === 'settings') return
      if (!loadedAt || Date.now() - Date.parse(loadedAt) >= settings.refreshSec * 1000) load($)
    })
    return next(e)
  })

  on('command.run', { command: 'pad-pane' }, async ($) => {
    if (open) {
      await $.ui.close({ id: PANE })
      open = false
      return {}
    }
    view = settings.startView
    stack.length = 0
    mode = null
    open = true
    await $.ui.open(OPEN_ARGS)
    // The command runs while its own text is still in the prompt, and a pane only
    // takes the keys from an empty prompt: ask again once the prompt has cleared.
    $.clock.after(150, () => { if (open) $.ui.open(OPEN_ARGS) })
    load($)
    return {}
  })

  // Esc steps back one level (an open form, an item, a collection, another
  // view); at the top level it closes the pane. Claude Code reports Esc as the
  // person closing the pane, which a hook may refuse.
  on('ui.close', async ($, e, next) => {
    if (e.id !== PANE) return next(e)
    if (e.origin?.kind === 'person' && stepBack($)) {
      $.clock.after(50, () => { if (open) $.ui.open(OPEN_ARGS) })
      // ui.close yields void: answering { value } without next keeps the pane
      // open. A bare {} is the wrong shape and the engine prints it to the chat.
      return { value: undefined }
    }
    open = false
    return next(e)
  })

  const remember = (k) => {
    ring.cur = k
    if (k && LIST_KEY.test(k)) ring.lastRow[view + ':' + (browseCollection || '')] = k
  }

  // Tab / Shift+Tab: one step through the menu, the list counting once (tabStep).
  on('ui.focus', async ($, e, next) => {
    if (e.requestId !== PANE) return next(e)
    const step = e.origin?.kind === 'person' && ring.terminal
      ? tabStep(ring.order, ring.cur, e.element, firstRowFor())
      : null
    if (!step) { remember(e.element); return next(e) }
    remember(step.element)
    if (step.engineStop) {
      // the engine won't retarget a move headed for its own stop: keep the ring,
      // then move it ourselves
      $.clock.after(0, () => $.ui.focus({ requestId: PANE, key: step.element }).catch(() => {}))
      return {}
    }
    // an engine stop's event names no plugin; a redirect must name ours
    return next({ ...e, plugin: e.plugin || PLUGIN, element: step.element })
  })

  // Up / Down: a one-row scroll becomes a move to the next row (arrowStep). Page
  // keys, Home/End and fast wheel spins still scroll.
  on('ui.scroll', async ($, e, next) => {
    if (e.requestId !== PANE || e.origin?.kind !== 'person' || !ring.terminal) return next(e)
    const to = arrowStep(ring.order, ring.cur, e.by, firstRowFor())
    if (!to) return next(e)
    remember(to)
    $.ui.focus({ requestId: PANE, key: to }).catch(() => {})
    return {}
  })

  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
    if (e.requestId !== PANE) return next(e)
    return draw($, e)
  })

  // Keep an open pane fresh when Claude changes Pad, through the CLI or MCP.
  on('tool.call', async ($, e, next) => {
    const result = await next(e)
    // Never let the pane's refresh disturb the tool call it watched.
    try {
      if (!open || busy || mode) return result
      const viaCli = e.tool === 'Bash' && /\bpad\s+(item|project|collection)\b/.test(String(e.command || ''))
      const viaMcp = /^mcp__.*pad.*__pad_(item|project|collection)$/i.test(String(e.tool || ''))
      if (viaCli || viaMcp) load($).catch(() => {})
    } catch {}
    return result
  })
}
