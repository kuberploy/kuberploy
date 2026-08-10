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
