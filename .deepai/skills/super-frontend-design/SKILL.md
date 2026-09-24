---
name: super-frontend-design
description: Create distinctive, production-grade frontend interfaces with professional iconography and real photography. Use this skill when the user asks to build web components, pages, artifacts, posters, or applications that demand professional visuals. Replaces emoji icons with Lucide/Tabler SVG icons and gradient-only backgrounds with curated Pexels photography.
---

This skill guides creation of distinctive, production-grade frontend interfaces that avoid generic "AI slop" aesthetics. It enforces two hard constraints that separate amateur designs from professional ones:

1. **Professional Icons Only** — no emojis, ever. Use Lucide Icons or Tabler Icons as inline SVG.
2. **Real Photography & Illustrations** — no bare gradient backgrounds as the primary visual. Source high-quality, context-relevant images from Pexels.

Implement real working code with exceptional attention to aesthetic details, creative choices, and defensive integration of third-party assets.

## Required Configuration

Photography comes from real Pexels API responses, never from memory:

- With `PEXELS_API_KEY` in the environment, query the API at generation time (curl or fetch) and embed the URLs it actually returns.
- Without a key, do NOT invent `images.pexels.com` URLs — a made-up photo ID renders as a broken image. Pick an aesthetic that needs no photography (typographic, illustrative, CSS/SVG texture and pattern) and say so in the reply.
- Never write the key into generated code or commit it.

## Design Thinking

Before coding, understand the context and commit to a BOLD aesthetic direction:
- **Purpose**: What problem does this interface solve? Who uses it?
- **Tone**: Pick an extreme: brutally minimal, maximalist chaos, retro-futuristic, organic/natural, luxury/refined, playful/toy-like, editorial/magazine, brutalist/raw, art deco/geometric, soft/pastel, industrial/utilitarian, etc. There are so many flavors to choose from. Use these for inspiration but design one that is true to the aesthetic direction.
- **Constraints**: Technical requirements (framework, performance, accessibility).
- **Differentiation**: What makes this UNFORGETTABLE? What's the one thing someone will remember?

**CRITICAL**: Choose a clear conceptual direction and execute it with precision. Bold maximalism and refined minimalism both work - the key is intentionality, not intensity.

Then implement working code (HTML/CSS/JS, React, Vue, etc.) that is:
- Production-grade and functional
- Visually striking and memorable
- Cohesive with a clear aesthetic point-of-view
- Meticulously refined in every detail

## Iconography Rules (Hard Constraint)

- **NEVER use emoji** for UI icons, feature bullets, buttons, cards, navigation, or decorative elements.
- Use **Lucide Icons** (https://lucide.dev) or **Tabler Icons** (https://tabler-icons.io) as inline SVG.
- Choose icons that match the semantic meaning of the element. Do not pick icons arbitrarily.
- Use current icon names (`Pencil`, `CircleCheck`, `ChartColumn`); older aliases like `Edit3`, `CheckCircle2`, `BarChart3` are gone from recent Lucide releases.
- Inline SVG is preferred over icon fonts for performance, accessibility, and crisp rendering.
- Apply consistent stroke width (usually `1.5` or `2`), size (usually `16px`, `20px`, or `24px`), and color (currentColor or CSS variable).
- Provide `aria-hidden="true"` on decorative icons and meaningful `aria-label` on interactive icon-only controls.
- If using React/Vue/Svelte, import the icon from the official package or render the SVG directly. Do not use `<img>` for simple UI icons.

### Recommended Icon Patterns

- Navigation: `Menu`, `X`, `Home`, `Search`, `User`, `Settings`
- Actions: `ArrowRight`, `ArrowUpRight`, `ChevronDown`, `Plus`, `Trash2`, `Pencil`, `Copy`
- Status: `CircleCheck`, `CircleAlert`, `Info`, `CircleX`, `LoaderCircle` (animated spin)
- Features: `Zap`, `Shield`, `Globe`, `Layers`, `Sparkles`, `ChartColumn`

## Imagery Rules (Hard Constraint)

- **NEVER leave a design with only gradient backgrounds** as the visual layer. Gradients are allowed as accents, overlays, or subtle atmosphere, not as the sole visual.
- Every major section (hero, feature highlight, testimonial, CTA, about) must carry a visual layer: photography on marketing/landing/poster-style deliverables; a deliberate texture, pattern, or illustration route is equally valid for tools, dashboards, and internal UIs.
- Source images from **Pexels** (https://www.pexels.com) via the official API. Every embedded `images.pexels.com` URL must come from a real API response or pass an HTTP check (200) — never a photo ID recalled from memory.
- Prefer images that match the tone: editorial photography for luxury, candid photography for human products, architectural for SaaS, nature/texture for wellness, tech/abstraction for developer tools.
- Always provide `alt` text. Decorative images should have empty `alt=""` and `role="presentation"`.
- Use responsive images: `srcset`, `loading="lazy"`, and `decoding="async"` for below-the-fold images; `fetchpriority="high"` and eager loading for the hero.
- Apply image overlays (subtle gradients, color tints, duotone filters) to preserve text legibility and unify the palette.
- Credit photographers when legally/technically appropriate; Pexels content is free to use without attribution, but attribution is a professional courtesy.

## Frontend Aesthetics Guidelines

Focus on:
- **Typography**: Choose fonts that are beautiful, unique, and interesting. Avoid generic fonts like Arial and Inter; opt instead for distinctive choices that elevate the frontend's aesthetics; unexpected, characterful font choices. Pair a distinctive display font with a refined body font.
- **Color & Theme**: Commit to a cohesive aesthetic. Use CSS variables for consistency. Dominant colors with sharp accents outperform timid, evenly-distributed palettes.
- **Motion**: Use animations for effects and micro-interactions. Prioritize CSS-only solutions for HTML. Use Motion library for React when available. Focus on high-impact moments: one well-orchestrated page load with staggered reveals (animation-delay) creates more delight than scattered micro-interactions. Use scroll-triggering and hover states that surprise. Wrap all decorative motion in a `prefers-reduced-motion: reduce` fallback that leaves the page fully usable.
- **Spatial Composition**: Unexpected layouts. Asymmetry. Overlap. Diagonal flow. Grid-breaking elements. Generous negative space OR controlled density.
- **Backgrounds & Visual Details**: Create atmosphere and depth rather than defaulting to solid colors. Add contextual effects and textures that match the overall aesthetic. Apply creative forms like geometric patterns, layered transparencies, dramatic shadows, decorative borders, custom cursors, and grain overlays — but always anchored by real photography or illustration.

NEVER use generic AI-generated aesthetics like overused font families (Inter, Roboto, Arial, system fonts), cliched color schemes (particularly purple gradients on white backgrounds as the only background), predictable layouts and component patterns, and cookie-cutter design that lacks context-specific character.

Interpret creatively and make unexpected choices that feel genuinely designed for the context. No design should be the same. Vary between light and dark themes, different fonts, different aesthetics. NEVER converge on common choices (Space Grotesk, for example) across generations.

**IMPORTANT**: Match implementation complexity to the aesthetic vision. Maximalist designs need elaborate code with extensive animations and effects. Minimalist or refined designs need restraint, precision, and careful attention to spacing, typography, and subtle details. Elegance comes from executing the vision well.

## Pexels Integration Pattern

The default is generation-time search: query the API once while building the deliverable and embed the returned `images.pexels.com` URLs directly in the markup. Shipped code contains image URLs only — no API key, no runtime dependency on Pexels.

Only when the deliverable is an app with a Node backend that needs dynamic, user-driven search, generate a server-side helper:

```js
const API_KEY = process.env.PEXELS_API_KEY;
const BASE = 'https://api.pexels.com/v1';

export async function searchPhotos(query, { perPage = 6, orientation } = {}) {
  if (!API_KEY) {
    throw new Error('PEXELS_API_KEY is not configured');
  }
  const params = new URLSearchParams({ query, per_page: String(perPage) });
  if (orientation) params.set('orientation', orientation); // landscape, portrait, square
  const res = await fetch(`${BASE}/search?${params}`, {
    headers: { Authorization: API_KEY },
  });
  if (!res.ok) throw new Error(`Pexels API error: ${res.status} ${res.statusText}`);
  return res.json();
}

export function pexelsSrc(photo, size = 'large') {
  // size: original, large2x, large, medium, small, portrait, landscape, tiny
  return photo?.src?.[size] || photo?.src?.original;
}
```

This module is server-side only. Never reference the key in client code — bundlers inline `VITE_`-style env vars straight into the shipped bundle; a client that needs dynamic search calls a backend route, not the Pexels API.

For static HTML/CSS deliverables, run the same search at generation time and embed the resulting `src.large2x` URL in the markup. A URL cited from memory (one you are confident Pexels previously returned) must be verified with an HTTP request first — drop it on anything but a 200.

## Deliverable Checklist

Before finishing, confirm the generated design:
- [ ] No emoji used as icons or decorative symbols.
- [ ] All icons are Lucide or Tabler SVG, consistently styled, current names.
- [ ] No section relies solely on a gradient background.
- [ ] Every major section carries a visual layer — Pexels photography on marketing-style pages, or a deliberate texture/illustration route on tool-style UIs.
- [ ] Every embedded `images.pexels.com` URL came from a real API response or passed an HTTP check — none invented.
- [ ] Images have `alt` attributes and responsive attributes where appropriate.
- [ ] No API key in shipped client code; keys stay in server-side environment variables.
- [ ] Decorative animation respects `prefers-reduced-motion`.
- [ ] Code is production-grade: accessible, responsive, and performant.

Don't hold back — show what a fully committed, distinctive, professional vision looks like.