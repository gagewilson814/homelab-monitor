# Dashboard UI Design — Research & Decisions

Working doc for the frontend. The repo README is the user-facing spec; this
captures *why* for the design choices and the backlog of improvements.

## 1. What we're designing for

A **single-user, mobile-first ops dashboard** for a home server fleet. Phone is
the primary access surface (the whole product pitch is "reachable from a phone
without exposing anything to the public internet"). Operators glance at it to
answer three questions:

1. **Is everything up right now?** (connection + service state)
2. **What's using resources?** (CPU / memory / disk)
3. **What needs my attention?** (alerts, threshold breaches, offline nodes)

The data model is small and static: hostname, CPU/mem/disk %, uptime, a list of
service checks, a `last_seen` timestamp, and per-agent online/offline state.
There is **no history or time-series** yet — every poll is a fresh snapshot.

## 2. Research: how established monitoring tools do it

I compared against the de-facto standards in this space — **Grafana**,
**Uptime Kuma**, **Netdata**, **Better Uptime / Checkly**, and **Statuspage** —
and extracted the conventions that apply to our constraints.

| Concern | What the leaders do | Why it works |
|---|---|---|
| **Status encoding** | Color **+** icon **+** label, never color alone | Colorblind-safe; glanceable at any size on a phone |
| **Metric rendering** | Sparkline (mini time-series) over a value, or gauge | Shows *trend*, not just a point-in-time number — the single biggest ops signal |
| **Layout density** | Compact grid of "widgets"; desktop = wide, mobile = stacked | Info density scales with viewport |
| **Alerting** | Badges + toast/inline banner + color, not just a background alert | Alert context stays with the offending node |
| **Empty/error/loading** | Distinct, calm states with a retry affordance | Failed polls shouldn't look like a crash |
| **Mobile nav** | Bottom tab bar or pull-to-refresh | Thumb-reachable; the phone is where you are |
| **Color semantics** | Green=ok, amber=warn, red=bad — consistent everywhere | One mental model across cards, alerts, nav |

Key takeaways that directly apply to us (detailed per-decision below):

- **Sparklines beat single bars.** A bar tells you the number today; a sparkline
  tells you whether it's climbing. For ops, trend is the actionable signal.
- **Never encode status by color alone.** Our current `.dot` already does color;
  pairing it with a text/shape label closes the colorblind gap cheaply.
- **Alerts should be contextual.** A Discord ping is great, but the dashboard
  itself should surface "1 node offline" so you don't have to open every card.
- **Consistent, semantic color ramp** (`--ok / --warn / --bad`) is already the
  right foundation — keep it and extend it rather than ad-hoc colors.

## 3. Decisions & backlog

### 3.1 Metric cards: add sparklines (top priority)

**Decision:** Replace the static CPU/mem/disk bar with a **sparkline + value**.

**Rationale:**
- The dashboard polls every 5s and *has* this data — we're just not storing it.
  A 60–120 point sparkline (1–2 min of history) makes trends visible.
- Grafana/Netdata's core value is exactly this: the line shape is the signal.
- Low cost: store `[]float64` per metric in the backend cache, render a `<polyline>`
  or SVG path. No new framework.

**Design:** horizontal sparkline under each metric label, value on the right,
color from the existing `barClass()` ramp (green/amber/red by level, or by the
sparkline's latest segment). Keep the bar as an optional "current level" accent.

**Backlog (later):** make sparkline length/time-window configurable; add per-card
history toggle.

### 3.2 Status: never rely on color alone (high)

**Decision:** Every status element = **color dot + text + icon**, consistently.

**Current state:** `.dot` uses color; offline cards use a red left border. This
is decent but inconsistent (dot vs border) and color-only.

**Design:**
- Online: green dot + "up"/"online" text.
- Offline / unreachable: red dot + "offline" text (not just a border).
- Service checks: keep the dot but add the word `up`/`down` (currently shown —
  good, keep it).
- Add `prefers-color-scheme: light` support so the semantic ramp survives on
  light backgrounds (Grafana-style). Status colors must stay distinct in light
  mode too.

### 3.3 Mobile-first chrome (high — this is the primary device)

**Decision:** Give the phone experience a proper bottom navigation + pull
semantics, not just a header bar.

**Design:**
- **Bottom tab bar** (thumb zone): `Overview | Nodes | Alerts | Settings`.
  "Alerts" aggregates offline nodes + threshold breaches + last-alert time.
- Sticky header with the live/error pill + node count.
- `touch-action: manipulation` and min 44px tap targets (WCAG-friendly).
- Card grid stays, but on narrow widths cards are full-width stacked (already
  `minmax(240px, 1fr)` — just confirm mobile stacks cleanly).

### 3.4 Aggregated alerts view (high)

**Decision:** Add an **Alerts** surface that lists problems without opening each
card. Backend already computes online/offline transitions and threshold alerts —
the dashboard should render them.

**Data needed from backend:** a flat `alerts` array with `{type, target, message,
since, acknowledged}`. The backend alert logic exists; the wire shape just needs
defining. This is the biggest UX win: "3 problems" is instantly visible.

**Design:** Alert list with type icon (offline / threshold / disk-full), a
`since` relative time, and an acknowledge/clear action. Keep it dense and
scannable.

### 3.5 Empty / loading / error states (medium)

**Current gaps:**
- On first load, `#fleet` is empty for ~5s before the first poll — shows a blank
  grid.
- A failed poll only shows "error: …" in the header pill; the fleet itself goes
  blank, which looks like a crash.
- Login error is fine, but no shake/retry affordance.

**Design:**
- **Loading skeleton**: gray placeholder cards matching the real card shape while
  the first poll lands (feels instant).
- **Error state**: when a poll fails, render a centered banner ("Can't reach
  backend · retrying…") with an explicit retry button, and keep the last-known
  fleet data visible instead of blanking it.
- **Empty state**: if no agents are configured / no data, show a calm message
  ("No agents yet") rather than an empty grid.

### 3.6 Offline agents: show more than "down"

**Current:** offline card shows address + error text + last-seen. Good.

**Improvement:** Surface *when* it went offline (use `last_seen` — already there)
and, if the backend can tell, *why* (TCP connect failure vs timeout vs DNS). The
backend already tracks last-seen, so "X minutes ago" is free. Consider a gentle
pulse animation on the red dot to draw the eye without being alarming.

### 3.7 Consistent color ramp + theming (medium)

**Keep** the `--ok / --warn / --bad` semantic ramp and `--accent` chrome
separation — this is the correct architecture.

**Extend:**
- Add a **light theme** (monitoring dashboards like Grafana default to dark, but
  light is important for daytime phone use).
- Add **reduced-motion** support: disable the status pulse / transition
  animations for users who prefer it (`@media (prefers-reduced-motion)`).
- Consider a **theme toggle** in Settings (dark/light/system).

### 3.8 Number formatting & locale (low, but correct)

- Percentages use `.toFixed(1)` — fine. Use `tabular-nums` (already applied to
  values) so sparkline values don't jitter.
- Uptime `3d 5h 12m` is good; consider adding a compact "1h2m" variant for
  dense views.
- Percentages/decimals should respect locale eventually (Intl.NumberFormat) —
  low priority for a single-user tool.

### 3.9 Accessibility & contrast (medium)

- Header pill text on colored backgrounds — verify contrast ratios (WCAG AA for
  the status colors against `--panel`).
- Ensure focus states are visible on all interactive elements (esp. bottom nav
  once added).
- `aria-live` region for the status pill so screen readers announce "live" /
  "error" changes.
- Icons should have text labels for state (don't rely on a dot alone).

### 3.10 Performance & architecture (low — already healthy)

- 5s poll of a cached JSON is fine. Keep vanilla JS (no framework overhead).
- Consider **request coalescing**: if a poll fails, don't hammer the backend with
  a tight retry storm — debounce retries (the SW offline fallback already helps
  the client side).
- Service worker is solid (network-first + cached fallback). Keep CACHE_VERSION
  discipline when the API shape changes.

## 4. Suggested prioritized roadmap

| # | Change | Effort | Impact | Notes |
|---|--------|--------|--------|-------|
| 1 | **Sparklines for CPU/mem/disk** | S | High | Needs backend to store per-metric history (new cache field) |
| 2 | **Alerts aggregation view** | S | High | Backend already computes transitions; define `alerts` wire shape |
| 3 | **Mobile bottom nav + pull-to-refresh** | S | Med | Pure frontend; big win on the primary device |
| 4 | **Loading skeletons + error/empty states** | S | Med | Pure frontend; removes "blank = crash" anxiety |
| 5 | **Status: color+text+icon everywhere** | XS | Med | Polish; closes colorblind/accessibility gap |
| 6 | **Light theme + reduced-motion** | S | Med | Daytime phone use; cheap once theming is split out |
| 7 | **A11y: aria-live status, focus states, contrast** | M | Med | Compliance; mostly additive |
| 8 | **Per-card history / sparkline window toggle** | L | Low | Nice-to-have after sparklines land |

**Recommended next step:** #1 (sparklines) + #2 (alerts view) together — they
turn the dashboard from "a wall of current numbers" into "a wall of *trends and
problems*", which is what ops actually wants to see. Both are feasible without a
framework and build on the existing alert/backend infrastructure.

## 5. Open questions for the team

- Do we store **per-metric history** (enables sparklines) or keep single
  snapshots? This is the one backend change that unlocks #1.
- Should "Alerts" be its own tab, or just an inline banner on Overview? (Inline
  is less code; a tab is more scannable on desktop.)
- Light theme now, or dark-only for now? (Dark is the ops default; light is a
  nice-to-have.)
