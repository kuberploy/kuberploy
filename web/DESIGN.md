# Kuberploy frontend direction

Kuberploy is an infrastructure console, not a marketing dashboard. Tailwind and
shadcn/ui provide implementation primitives; their example layouts and default
visual style are not the product design.

## Product character

- Dense enough for operators to compare state without scrolling between cards.
- Flat sections, tables, lists, and rules before boxed containers.
- Left-aligned, descriptive headings. No slogans or generic hero copy.
- Native system typography with tabular numerals for revisions, counts, and time.
- Neutral surfaces with one action accent. Semantic colors communicate state only.
- Light, dark, and live system appearance are equal first-class modes.

## Rejected patterns

- No gradients, glass effects, glow, decorative blobs, or reveal animations.
- No equal-card grids used merely to fill space.
- No rounded icon tile in every section.
- No all-caps eyebrow above every heading.
- No giant empty cards when a compact row or plain empty state is clearer.
- No pill shape unless the content is genuinely a status, tag, or compact filter.
- No untouched shadcn demo styling; component source must use Kuberploy tokens and
  the smallest practical radius.
- No color used only to make adjacent cards look different.

## Composition rules

1. Start with the operator task and the comparison they need to make.
2. Prefer one clear page title, a short factual description, and actions aligned
   to the same baseline.
3. Use a table/list when rows share a schema. Use a card only for a bounded object
   with independent actions or state.
4. Separate major sections with rules and spacing. Do not wrap every section.
5. Keep state, identity, revision, and next action visible without opening a modal.
6. Test at 1024px and mobile, in light and dark mode, with empty and populated data.

## Styling system

Tailwind CSS 4 plus a small set of shadcn primitives. There is one system, not
two: shadcn _is_ Tailwind, so the primitives and the app code share a vocabulary.

`src/styles.css` is ~500 lines and holds only four things: the layer order, the
token declarations on `:root` (and their dark-theme values), the `@theme inline`
block that gives every token a utility name, and a `@layer base` block for bare
elements the app renders directly — a plain `<input>`, `<textarea>`, `<a>` — plus
the keyframes the utilities reference. There are no component class rules. There
is nothing to keep in sync between a component and a stylesheet.

**Layer order is declared before Tailwind's import.** Unlayered CSS outranks
every layered rule regardless of specificity, so a plain `a { color: inherit }`
sitting outside a layer silently beat `text-accent-ink` on every link-shaped
button. `@layer theme, base, components, utilities;` on line 1 puts utilities
last, and a utility on an element now always wins.

Where the same utilities appear more than a couple of times, they live in a
component in `components/ui.tsx` — `Page`, `Card`, `CardHeader`, `FormCard`,
`FormGrid`, `Field`, `Notice`, `StatusPill`, `EmptyState`, `DetailList`,
`ConfigSection`. Pages compose those; they do not restate the utilities. The
three vendored primitives (`shadcn/button`, `shadcn/card`, `shadcn/dialog`) are
reached only through `ui.tsx`, so a primitive can be restyled or replaced in one
place.

For an element that must not be a `<button>` — a router `<Link>` styled as one —
use `buttonVariants({ variant })` rather than faking it with a `<button>` and an
`onClick` that navigates.

Components that other code needs to find — in a test, in a container query, in a
parent's descendant selector — carry a `data-slot` attribute, never a styling
class. Utilities churn as the design changes; `data-slot="page"` does not.

## Tokens

Every value below is declared on `:root` in `src/styles.css` and mapped to a
Tailwind utility name in the `@theme inline` block above it. Use the utility, not
a literal: `bg-surface`, not `bg-[#fff]`.

The tokens swap on `[data-theme="dark"]` and `@theme inline` points the utility
names at the tokens, so **this app writes no `dark:` variants**. `bg-surface`
is correct in both themes by construction. A hard-coded hex in a class is the
one thing that breaks that, and it is a review comment.

Tailwind's default spacing scale is already 4px x n, so `p-4` _is_ `--space-4`.
There is no separate spacing mapping to keep in sync.

### Spacing

`--space-1` 4px · `--space-2` 8px · `--space-3` 12px · `--space-4` 16px ·
`--space-5` 20px · `--space-6` 24px · `--space-8` 32px · `--space-10` 40px ·
`--space-12` 48px

Every `padding`, `margin`, and `gap` resolves to one of these. Layouts stay on a
4px rhythm so unrelated panels line up down the page. The only exceptions are
1px hairlines and the negative offsets that overlap a border.

### Radius

`--radius-control` 8px (buttons, inputs, chips) · `--radius-panel` 12px (cards,
sections) · `--radius-overlay` 16px (dialogs and popovers that float above the
page).

The Tailwind `--radius-*` scale that shadcn primitives read is aligned to the
same steps, so a shadcn control and a hand-written one have the same corner.

### Type

`--text-micro` 12px (metadata, hints, table captions) · `--text-meta` 13px
(labels, buttons, secondary copy) · `--text-body` 14px (body copy, row titles) ·
`--text-lead` 15px (subsection titles) · `--text-section` 17px (card titles).
Page titles use `clamp(24px, 2.4vw, 30px)`.

**12px is the readability floor.** Nothing smaller than 11px ships, and 11px is
reserved for uppercase eyebrows and nav section labels.

**A parent never renders smaller than its own child.** A row's name outranks the
identifier under it; a section heading outranks its description. Inverting that
is the single most common regression in this codebase.

### Weight

400 body · 500 emphasis and labels · 600 headings and buttons · 700 only for
avatar and badge initials. Nothing above 700. Tracking stays at or above
`-0.02em`; tighter reads as squashed at these sizes.

### Color

`--ink` / `--ink-soft` / `--ink-faint` for text, `--line` / `--line-strong` for
borders, `--surface` / `--surface-soft` / `--canvas` for grounds, `--mint` and
`--mint-dark` for the single action accent, `--mint-line` for a border on a mint
ground, and `--blue` / `--amber` / `--red` for state only.

**A state fill is not a text color.** `--mint`, `--amber`, and `--red` are tuned
to sit behind something. Text uses the readable pair: `--amber-ink`, and for
filled controls `--accent-surface` / `--accent-ink` and `--danger-surface` /
`--danger-ink`. Those pairs invert between themes rather than tracking a single
hue — the dark accent is a light mint carrying dark ink, because white on it is
1.9:1.

Every text/background pair must clear WCAG AA: 4.5:1, or 3:1 at 24px or at
18.7px bold. Check both themes; a pair that passes in light routinely fails in
dark once the surface flips.

A literal hex in a rule is a dark-mode bug waiting to happen: it will not flip.
Code surfaces that are deliberately dark in both themes (the log viewer, the
rendered-manifest preview) are the documented exception. Anything wrapping a
theme-aware child — the Monaco YAML editor, for instance — must follow the theme
too, or light mode shows a light editor inside a black frame.

## Layout rules

7. Grid tracks must match the number of children. A five-control row in a
   three-column grid overflows; a heading without its icon lands in the icon
   track. Check both when reusing a row class.
8. An `auto` grid track still stretches its child. A button beside a select needs
   `justify-self: start` or it renders the width of the row.
9. Dense rows wrap before the phone breakpoint. Six and seven column rows stop
   being readable around 1240px; collapse them in stages, not once at 580px.
10. Loading and permission-guard branches render inside the page container. A
    bare `<Skeleton>` or `<EmptyState>` returned from a page component sits
    edge-to-edge with no padding.
11. An empty state inside a card draws no border of its own. The card is the
    boundary.
12. `<Page>` and `<PageStack>` own the vertical rhythm: one 24px grid gap
    between sections, on every route. A section never adds its own
    `margin-bottom` to reach for more room.

## Motion

Three durations, two easings, all tokens on `:root` and reachable as
`duration-(--motion-fast)` / `ease-(--ease-standard)`. Before them the sheet held
100, 120, 150 and 180ms side by side on controls that share a row, so a hover
crossing two of them read as uneven.

- `--motion-fast` (120ms) — a control reacting under the pointer: hover, press,
  focus ring, checked state.
- `--motion-base` (180ms) — something entering or leaving: a route, a scrim, a
  notice, a tab marker, the off-canvas sidebar.
- `--motion-slow` (260ms) — reserved for a panel-sized move.
- `--ease-standard` decelerates into rest. `--ease-exit` does the reverse, so a
  leaving element clears the way quickly.

Rules:

1. Motion only where a state change would otherwise be instantaneous and hard to
   follow. Nothing loops except a genuine progress indicator, and nothing delays
   an interaction.
2. A route enter is a 4px rise plus a fade on `<Page>`. The keyframe ends on
   `transform: none` so it leaves no containing block behind for the absolutely
   positioned editor chrome inside.
3. A selection marker grows from the leading edge of the thing selected. It is
   the only cue that says which tab was picked.
4. Press feedback on every button. Without it a button that starts a slow
   mutation looks like it ignored the click until the busy state arrives.
5. The `prefers-reduced-motion` block reduces every rule above to ~0ms. Nothing
   here may be load-bearing for comprehension.

## Responsive rules

1. Breakpoints are the named `to-*` variants declared in `styles.css`, not
   Tailwind's `max-[Npx]:`. `max-[820px]:` compiles to `width < 820px`, which is
   one pixel off the `max-width: 820px` these layouts were drawn against, so at
   exactly 820px the wrong branch wins. `to-820:` is the inclusive form. They
   are declared widest-first, because Tailwind emits variants in declaration
   order and the narrower query has to win where two of them overlap.
2. A `to-820:` variant reads the viewport, but the docked sidebar takes
   240px off the content column. An 834px tablet gives panels the room of a
   ~530px phone and no viewport breakpoint fires. Anything that lays out
   _inside_ the page column is therefore a container query:
   `page-to-560:`. `<Page>` declares `@container/page`, so the variant
   measures the column the content is actually in.
3. A wrapped column flex lays its lines out horizontally: each line is sized to
   its widest item's max-content, and `stretch` stretches to the line, not to
   the container. When a flex row becomes a stack at a breakpoint, turn wrapping
   off in the same rule.
4. A grid container needs an explicit `grid-cols-[minmax(0,1fr)]` track. The implicit track
   takes `min-width: auto` from its widest child, so one long identifier sizes
   the column past the box it sits in.
5. Anything that can hold an identifier — headings, eyebrows, header
   descriptions — sets `break-words`. An image reference or revision
   hash is one unbreakable token: left alone it overflows its box invisibly and
   drags the whole page into horizontal scroll.
6. Touch targets are widened with `pointer-coarse:`, never by width.
   A tablet at 834px has a coarse pointer; a 768px window on a laptop does not.
   32px minimum, 40px for tab strips. Desktop density is untouched.

## Affordances

- Any value an operator cannot retype — an image digest, a config revision, a
  target ID, a deploy key, a confirmation phrase — ships with `CopyButton` or
  `CopyableCode` from `components/ui`.
- Clipboard access goes through `useCopyToClipboard` (`lib/clipboard.ts`), which
  falls back to a selection copy. Self-hosted control planes are often reached
  over plain HTTP, where `navigator.clipboard` does not exist.
- A destructive confirmation names the phrase, makes it copyable, reports a
  mismatch while typing, and confirms on Enter once it matches.
- One current-page highlight per navigation. A section parent gets
  `nav-link--section`, not the accent treatment its child is using.
- An empty state carries an action when there is a real one and the operator has
  the capability for it — the same destination and the same gate as the header
  CTA for that page. A state that is empty because of a filter offers the way
  back out of the filter. A capability denial and a "pick something first"
  prompt carry no action; there is nothing for the button to do.
- Navigation is ordered by what an operator is doing, not by when each page was
  added: observe the platform, manage who is on it, then the configuration that
  shipping depends on, then cluster plumbing, then the platform's own lifecycle.
  Platform releases sits last — least frequent, and the only entry that changes
  the platform itself.
- One glyph, one meaning. Four settings entries shared the `route` icon and read
  as the same kind of thing; certificates and DNS got their own.

## Roles

One tab pattern: `<nav>` plus plain buttons with `aria-current="page"`. It is a
navigation, it reads as one, and it promises nothing it does not do.

`role="tablist"` is reserved for a real tablist and comes with the keyboard
behaviour a screen reader is told to expect: one tab stop for the whole strip,
arrow keys to move between tabs, Home/End to the ends. `useRovingFocus` in
`ui.tsx` supplies exactly that and skips disabled tabs.

A control that cannot be operated does not get an interactive role. An App's
source kind is fixed at creation, so the four kinds are a list with the current
one marked `aria-current`, not four tabs of which three are disabled.

## React rules

No `memo` or `useCallback` anywhere, so inline handlers cost nothing -- there is
no memoized child for a new function identity to defeat.

1. Keys are unique **among siblings**, including static JSX siblings that are
   not in an array. Two panels in one wrapper both keyed on `project.id` is the
   same collision as two rows in a `map`, and React warns about both.
2. A user-editable name is never a key. It is not unique until the validator
   says so, and the editor's job is to let the operator reach the state where it
   is not. Neither is a position: a key is React's identity for a row, so an
   index key hands a deleted row's DOM node -- with its focus, its selection and
   its uncontrolled value -- to whichever row slid into that position.
3. Where the persisted shape carries no id -- a Traefik chain entry is a bare
   string, a scheduling rule is a YAML-shaped object -- `useRowKeys` in `ui.tsx`
   mints identity per row and moves it with the row. Callers must tell it about
   a removal (`removeAt`) or a reorder (`moveRow`); it cannot infer either from
   a length change. `ui.test.tsx` locks down the removal case.
   `TraefikMiddlewareEditor` additionally hands focus back to the control for
   the entry that moved, because at either end of a chain the button that made
   the move is disabled in its new position.
4. A selection is a preference, not a fact. Components hold the id the operator
   picked (`...Choice`) and derive the effective selection from the list that
   exists in this render, so a list that loses the picked entry falls back
   inside the same render. Clamping in an effect paints the invalid selection
   first and corrects it one render later.
5. Resetting state when an identity changes is the parent's `key`, not a child
   effect. `key` resets refs and in-flight mutation state too, which a
   hand-written reset effect has to remember to do. Where the reset covers more
   than one input -- an App plus its build platform -- the key covers all of
   them.
6. Effects that remain are the ones that talk to something outside React: an
   `EventSource`, Monaco, a `MutationObserver`, a one-time POST. A `setState`
   in an effect that only reads other React state is a render behind.
