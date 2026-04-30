# Pad Brand Spec

**Audience:** developers working on `pad-web` (`../pad-web`, the `getpad.dev`
marketing site) and `web/` (the SvelteKit app embedded in this repo's Go
binary, served at `app.getpad.dev` in Cloud mode and at `localhost:7777`
or operator domains in self-hosted mode).

**Purpose:** define the visual contract for surfaces that **border**
between marketing and product, so the two codebases can converge
intentionally rather than drift accidentally.

**This doc is _not_ a full design system.** It only covers what is
actually shared. The interior of the Pad app (the workspace shell, item
detail pages, board views, etc.) keeps its own conventions and is
explicitly out of scope here.

> Tracked under [PLAN-900] (Cohesive UX between getpad.dev and Pad
> Cloud), TASK-904. When you change anything in this doc, update the
> citations in the source files listed under [How both repos cite this
> doc](#how-both-repos-cite-this-doc).

---

## 1. Where parity matters / where divergence is fine

This is the most important decision in this document. Read this section
first.

| Surface | Parity? | Notes |
|---|---|---|
| `getpad.dev` marketing pages (home, docs, blog, legal) | — | The reference. Other surfaces converge toward this. |
| `app.getpad.dev` auth-page family (login, register, forgot-password, reset-password, join, OAuth-error) **in Cloud mode** | **Full parity** | Header + footer + tokens match `getpad.dev`. See PLAN-900 / TASK-902 / TASK-903. |
| `app.getpad.dev` 404 / 500 error pages **in Cloud mode** | **Full parity** | Same chrome as auth pages. See TASK-906. |
| Transactional emails (invite, password reset, etc.) **in Cloud mode** | **Full parity** | Header + footer treatment + token palette. See TASK-907. |
| In-app **Resources** menu (links to docs / changelog / GitHub / support) | Brand-consistent text + icon treatment, but lives inside the workspace shell — does not import marketing chrome. See TASK-905. |
| Self-hosted auth pages, error pages, emails | **Neutral — no parity** | Operators ship Pad under their own brand. Self-hosted UI must NOT carry getpad.dev branding. Gated on the existing `cloud_mode` flag (see [Cloud-mode flag](#cloud-mode-flag)). |
| App workspace shell (TopBar, sidebar, item detail, board, list views) | **Divergent — keep as-is** | Tool aesthetic. Don't homogenize. |

**Two-line summary:** Cohesion applies at the seams between marketing
and product, and only when the user is on Cloud. Inside the app and on
self-hosted, leave things alone.

---

## 2. Cloud-mode flag

Every parity decision in this doc is conditional on Cloud mode. The
flag is already plumbed end-to-end and **no new env var should be
introduced**:

- **Server:** `cloud_mode` is returned on `/api/v1/auth/session`
  (see `internal/server/handlers_auth.go`).
- **Client:** `cloud_mode?: boolean` in `web/src/lib/api/client.ts`
  (lines 119, 127). Auth pages already read it
  (`web/src/routes/login/+page.svelte` line 18,
  `cloudMode = $state(false)`).
- **Components:** `LegalFooter` and `SupportFooter` accept a
  `cloudMode` prop today and switch behavior on it
  (`web/src/lib/components/auth/`).

Reuse this signal everywhere. If a new component needs to know whether
it's on Cloud, plumb the same prop — do not add a parallel mechanism.

---

## 3. Color tokens

The marketing site (`pad-web/src/app.css`) is the canonical source
for bordering surfaces. The accent palette is already aligned across
both repos; the structural tokens (bg / text / border) currently
diverge and the convergence direction is **app moves toward marketing**,
not the other way around.

### Canonical values (bordering surfaces only)

```
--color-bg              #111113   /* primary surface */
--color-bg-raised       #1a1a1e   /* card surface */
--color-bg-surface      #222226   /* nested surface */
--color-bg-hover        #2a2a30

--color-text            #ededef   /* body text */
--color-text-secondary  #a0a0a8   /* nav links, secondary copy */
--color-text-muted      #8a8a93   /* footer links, captions     ←  AA-passing */

--color-border          #2a2a30   /* card borders, dividers */
--color-border-subtle   #1f1f24   /* header / footer separators */

--color-accent          #4a9eff   /* primary action, focus ring */
--color-accent-hover    #3b8de6
--color-green           #4ade80
--color-amber           #fbbf24
--color-purple          #a78bfa
--color-red             #f87171
```

### Accent palette parity

The accent values (blue, green, amber, purple) are **already identical**
between `pad-web/src/app.css` and `web/src/app.css`. Treat them as fixed
and do not re-pick them.

### Known drift to clean up (out of scope for this doc)

- `web/src/app.css` defines `--text-muted: #666666`, which gives
  ≈3.2:1 against `--bg-primary: #1a1a1a` and **fails WCAG AA for
  body-size text**. The marketing site uses `#8a8a93` (≈4.7:1) which
  passes. When the auth-page header/footer convergence work
  (TASK-902 / TASK-903) lands, surfaces that consume the marketing
  palette automatically inherit the AA-passing value. The deep-app
  fix is a separate concern — file a follow-up if it bites.

---

## 4. Type scale

### Font families

Bordering surfaces use:

```
--font-sans  'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif
--font-mono  'JetBrains Mono', 'SF Mono', 'Fira Code', monospace
```

The deep app (`web/src/app.css`) currently uses system-ui as primary —
that's intentional for the workspace shell (system feel inside a tool)
and stays as-is. **Bordering surfaces only** should pull in Inter +
JetBrains Mono so they feel continuous with the marketing site.

If Inter / JetBrains Mono aren't already self-hosted on the app side,
loading them costs an HTTP request — fine for auth pages, error pages,
and emails (low-frequency surfaces); not justifiable inside the
workspace shell.

### Sizes (Tailwind utility names — both repos use Tailwind)

| Use | Class | Computed |
|---|---|---|
| Logo wordmark | `text-lg font-bold tracking-tight` | 18px / bold / tightened tracking |
| Nav link | `text-sm` | 14px |
| Body copy | base (no class) | 16px |
| Footer copy | `text-sm` | 14px |
| Footer muted (legal) | `text-sm text-text-muted` | 14px in `--color-text-muted` |
| Copyright line | `text-sm text-text-muted` | 14px |

---

## 5. Spacing & layout

### Container

```
mx-auto max-w-6xl px-6
```

`max-w-6xl` = 72rem = 1152px. Same on both repos. Use this on every
header / footer / page-content container that's part of the bordering
surfaces.

### Header

```
fixed top-0 z-50 w-full border-b border-border-subtle bg-bg/80 backdrop-blur-xl
```

- Fixed to top of viewport, full width, stays above page content.
- 80% opacity background plus `backdrop-blur-xl` for the soft-glass
  effect. Don't drop the blur — the bordering surfaces match each other
  via this exact treatment.
- `border-b border-border-subtle` is the only divider.
- Vertical padding `py-4` (16px), horizontal `px-6` (24px) inside the
  `max-w-6xl` row.

### Main content offset

`<main class="pt-16">` — 64px top padding so content doesn't slide
under the fixed header. Every bordering surface needs this.

### Footer

```
border-t border-border-subtle
```

Inside container:

```
flex flex-col items-center justify-between gap-4 px-6 py-8 sm:flex-row
```

- Stacks vertically on mobile, lays out horizontally at the `sm:`
  breakpoint.
- Link row uses `flex flex-wrap items-center justify-center gap-x-6 gap-y-3`.

### Mobile breakpoint

Tailwind's `md:` (768px). Above this, full nav; below, hamburger menu.
Both repos must use the same breakpoint to avoid awkward jumps when a
user opens the app from a marketing link on a tablet.

### Hamburger spec

- Closed: 24x24 SVG, two horizontal lines (`y=8` and `y=16`).
- Open: 24x24 SVG, X mark (lines from corner to corner).
- Stroke width 2, round caps and joins, `currentColor` so it inherits.
- Toggling the menu reveals an inline panel with the same nav links
  stacked, `border-t border-border-subtle px-6 py-4` for the panel.

(Pulled from the existing `pad-web/src/routes/+layout.svelte`
implementation — see lines 60–127. The auth-page header in this repo
must match this byte-for-byte for the SVG path so the visual
transition is identical.)

---

## 6. Header pattern

### Anatomy (left to right)

1. **Wordmark** — `<a href="/" class="text-lg font-bold tracking-tight text-text">pad</a>`.
   Lowercase. Always links to root of the current property
   (`getpad.dev/` on marketing, `app.getpad.dev/login` on auth pages
   pre-auth, etc.).
2. **Spacer** — `flex items-center justify-between` distributes.
3. **Link row (desktop)** — `hidden items-center gap-8 md:flex` containing
   nav links. Each link `text-sm text-text-secondary transition-colors hover:text-text`.
4. **Hamburger button (mobile)** — `flex items-center justify-center md:hidden`,
   aria-labelled "Toggle menu", `aria-expanded` reflects state, `aria-controls="mobile-menu"`.

### Link list — what goes in the header

The header link list is **not the same** between marketing and auth
pages. Don't try to homogenize it.

| Surface | Header links |
|---|---|
| `getpad.dev` (marketing) | Docs, Blog, GitHub, **Login** *(added by TASK-901)* |
| `app.getpad.dev` auth pages, Cloud mode | Docs, Blog, GitHub *(but linking to `https://getpad.dev/...`)* |
| `app.getpad.dev` auth pages, self-hosted | **No header at all** — render `null` |

Marketing nav adds a Login CTA because that's its job. Auth-page nav
omits it because the user is already on /login. Both nav surfaces
otherwise mirror each other so the user feels they're on the same
property.

### External links

Anything pointing off the current property opens in a new tab:

```svelte
<a href={link.href} target="_blank" rel="noopener noreferrer" ...>
```

`rel="noopener noreferrer"` is required — both for security
(`noopener` prevents the new page from accessing `window.opener`) and
because Tailwind/Svelte tooling will warn without it.

---

## 7. Footer pattern

### Anatomy

```
<footer class="border-t border-border-subtle">
  <div class="mx-auto flex max-w-6xl flex-col items-center justify-between gap-4 px-6 py-8 sm:flex-row">
    <p class="text-sm text-text-muted">
      © 2026 Pad
      <span class="mx-1">·</span>
      <a href="https://perpetualsoftware.org" target="_blank" rel="noopener noreferrer" class="text-text-muted/60 transition-colors hover:text-text-secondary">
        Perpetual Software
      </a>
    </p>
    <div class="flex flex-wrap items-center justify-center gap-x-6 gap-y-3">
      <!-- link list — see canonical order below -->
    </div>
  </div>
</footer>
```

### Canonical link order

Always the same order. This is the contract:

1. GitHub *(external)*
2. Docs
3. Changelog
4. Contribute
5. FAQ
6. Security
7. Privacy
8. Terms
9. Sub-processors

If a surface omits a link (e.g. self-hosted footer skips Changelog
because there's no Changelog page on a self-hosted install), keep the
*relative* order intact — don't reshuffle.

### Link styling

```
text-sm text-text-muted transition-colors hover:text-text-secondary
```

Hover lifts from `--color-text-muted` (`#8a8a93`) to
`--color-text-secondary` (`#a0a0a8`). Subtle by design — the footer is
secondary navigation, not a CTA strip.

### Copyright line

- Format: `© <current year> Pad · Perpetual Software`
- The middot is `&middot;` (`·`). Not `|`, not `-`, not `/`.
- "Perpetual Software" links to `https://perpetualsoftware.org` with
  the muted treatment shown above.
- Year is the current calendar year. If you're updating this doc, also
  audit the inline `2026` in `pad-web/src/routes/+layout.svelte` line 142.

### Self-hosted footer

When `cloud_mode === false`, render only the legal-essential subset:

- Privacy *(if the operator has hosted a privacy doc)*
- Terms *(if applicable)*
- A copyright line that says the operator's name, not Pad's (this is
  out of scope for the immediate cohesion work — for now keep the
  current self-hosted footer behavior; flag for the operator-branding
  follow-up plan).

---

## 8. How both repos cite this doc

When you change tokens, header structure, or footer link list in this
doc, update the source files that follow this contract. To make drift
auditable, each consuming file carries a one-line comment pointing
back here.

**Source of truth: `pad-web/src/routes/+layout.svelte`**

```svelte
<!-- Visual contract: docs/brand.md (Pad repo). When changing tokens,
     header structure, or footer link list, update both this file and
     the auth-page header/footer in the Pad repo to match. -->
```

**App-side consumers (each gets a similar comment):**

- `web/src/lib/components/auth/AuthHeader.svelte` *(new in TASK-902)*
- `web/src/lib/components/auth/LegalFooter.svelte`
- `web/src/lib/components/auth/SupportFooter.svelte`
- `web/src/routes/+error.svelte` *(touched by TASK-906)*
- Email templates in `internal/email/` *(touched by TASK-907)*

The comment doesn't need to be elaborate — one line citing the path is
enough. The point is that anyone touching one file can grep for the
others and keep them in sync.

---

## 9. What this doc explicitly does NOT do

- It is not a full design system. The deep app (workspace shell, item
  views, etc.) has its own conventions and stays as-is.
- It does not extract a shared component package. Both repos
  hand-implement to this contract. The cost of an `@pad/ui` package
  isn't justified by the small surface area being shared.
- It does not define operator-customizable branding for self-hosted.
  That's a separate, larger plan to be opened after PLAN-900 ships.
- It does not address mobile / responsive behavior inside the
  workspace shell — only at the bordering surfaces.

---

## 10. References

- [PLAN-900] — Cohesive UX between getpad.dev and Pad Cloud
- [IDEA-888] — Original idea this plan implements
- `pad-web/src/app.css` — canonical token values
- `pad-web/src/routes/+layout.svelte` — canonical header + footer
  reference implementation
- `web/src/app.css` — app-side tokens (currently divergent on
  bg / text / border / fonts; aligned on accent palette)
- `web/src/lib/components/auth/` — auth-page chrome components

[PLAN-900]: # "tracked in Pad workspace"
[IDEA-888]: # "tracked in Pad workspace"
