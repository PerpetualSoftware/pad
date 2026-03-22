# Pad Web UI

SvelteKit 2 + Svelte 5 frontend for Pad, compiled to static files and embedded into the Go binary.

## Development

```bash
npm install
npm run dev          # Dev server at localhost:5173 (proxies API to localhost:7777)
npm run build        # Production build to build/
npm run check        # Type checking with svelte-check
```

When developing, run the Go backend separately with `make dev` from the project root.

## Building for Production

**Do not build in isolation.** Always use `make build` from the project root — this builds the web frontend, then compiles the Go binary with the build output embedded via `//go:embed`.

## Stack

- **Svelte 5** with runes (`$state`, `$derived`, `$effect`)
- **SvelteKit 2** with `adapter-static` (SPA mode)
- **Tiptap** block editor with markdown round-trip
- **svelte-dnd-action** for drag-and-drop in board/list views
- **SSE** for real-time updates
- **TypeScript** throughout

## Structure

```
src/
  routes/                    SvelteKit pages
    +layout.svelte           App shell (sidebar + main)
    +page.svelte             Landing/redirect
    [workspace]/
      +page.svelte           Dashboard (collections, phases, activity)
      +layout.svelte         SSE connection per workspace
      [collection]/
        +page.svelte         Collection view (board/list)
      [collection]/[item]/
        +page.svelte         Item detail + editor
      conventions/            Purpose-built conventions page
      playbooks/              Purpose-built playbooks page
      settings/               Workspace settings
  lib/
    api/client.ts            HTTP API client
    components/
      layout/                Sidebar, navigation
      editor/                Tiptap editor, raw markdown editor
      fields/                FieldEditor, relation picker
      items/                 ItemCard, ItemDetail
      collections/           BoardView, ListView
      common/                StatusBadge, badges, modals
      search/                CommandPalette
      activity/              ActivityFeed
    stores/                  Svelte 5 reactive stores
      workspace.svelte.ts    Workspace state
      collections.svelte.ts  Collection + item state
      ui.svelte.ts           Sidebar, mobile state
    types/index.ts           TypeScript types and constants
  app.css                    Global styles and design tokens
```
