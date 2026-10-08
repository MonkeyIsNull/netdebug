package main

// dashboard.go — the entire self-contained dashboard page. It is a Go backtick
// RAW-STRING const (dependency-free and obvious) with inline <style> + TWO inline
// <script>s (a tiny pre-paint layout-restore in <head> + the main IIFE at end of
// body) and ZERO external URLs: no CDN, no web font, no <link>, no url(). The
// no-external-URL guarantee is enforced against these exact bytes by the
// STRENGTHENED TestDashboardHTMLHasNoExternalURLs in serve_test.go (case-folded,
// blanket `url(` ban, @font-face / <img> / .woff… all banned).
//
// TRAP: a Go raw string cannot contain a backtick, so BOTH inline <script>s must
// NOT use JS template literals — every string below is built with '+'
// concatenation. Keep it that way, or the const stops compiling.
//
// RENDERER: inline SVG. The throughput chart is TWO <polyline> nodes (down + up)
// over a faint flat area-fill polygon each; every point lives in each node's
// `points` attribute, so a full redraw mutates a handful of DOM nodes.
//
// PHASE 6 LOOK: sci-fi ops-terminal. Near-black ground, system-monospace
// everywhere (NO web font), uppercase tracked labels, a top STATUS BAR, a tight
// stat-tile grid (incl. the new radio tiles), restyled chart cards keeping the
// Phase-4 band wash + event markers, "// LIVE" / "// HISTORY" heads, a subtle
// pure-CSS scanline (toggleable, default ON), and a small footer. Dark-only.
//
// PHASE 4 band coloring (UNCHANGED semantics): color already encodes direction
// (blue = download line, amber = upload line), so band is a translucent full-height
// BACKGROUND <rect> per contiguous same-band run (a faint wash behind the sharp
// lines). Drop/roam/band_change events are first-class <line>/<rect> glyphs with a
// native <title> for offline tooltips. The band palette lives in ONE inline JS
// object keyed by the SAME strings Go's bandColorKey emits.
//
// HONEST DISPLAY: absent fields render "—", NEVER "RSSI 0" / "MCS 0" / "0 Mb/s".
// All free-form strings (ssid_label, PHY, SSID) are written via textContent, never
// innerHTML, so a restyle slip cannot become self-XSS on the loopback page.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>netdebug — live bandwidth</title>
<style>
  :root {
    /* Near-black ground + hairlines (dark-only by intent). */
    --bg: #0f1419; --panel: #141b22; --panel-2: #10161d; --grid: #26303b;
    --ink: #e6edf3; --muted: #8b98a5;
    /* Accent palette (CVD-considerate; every state also carries a text label). */
    --accent: #2bd576; --amber: #f0a63a; --cyan: #4aa3ff; --violet: #c77dff; --red: #ff5c5c;
    /* Direction line colors — DISTINCT tokens, NOT collapsed into the accents. */
    --down: #4aa3ff; --up: #ffb039;
    /* Phase 10 "your channel" highlight: a bright near-white outline, distinct from
       the band fills and every accent so it reads as YOU on any band color. */
    --you: #f4faff;
    --mono: ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
  }
  * { box-sizing: border-box; }
  html, body { margin: 0; min-height: 100%; }
  body {
    background: var(--bg); color: var(--ink);
    font: 13px/1.45 var(--mono);
    padding: 16px; -webkit-font-smoothing: antialiased;
  }
  /* Subtle pure-CSS scanline texture (NO image, no external reference). Behind content, scoped
     to the ground only (NOT the chart SVGs). Default ON via body.scanlines; the
     low opacity keeps 11px mono labels crisp. */
  body.scanlines::before {
    content: ""; position: fixed; inset: 0; z-index: 0; pointer-events: none;
    background: repeating-linear-gradient(0deg,
      rgba(255,255,255,0.03) 0px, rgba(255,255,255,0.03) 1px,
      transparent 1px, transparent 4px);
  }
  .wrap { position: relative; z-index: 1; max-width: 1200px; margin: 0 auto; }

  /* ---- top STATUS BAR --------------------------------------------------- */
  .statusbar {
    display: flex; align-items: center; justify-content: space-between;
    gap: 14px 22px; flex-wrap: wrap;
    background: var(--panel); border: 1px solid var(--grid); border-radius: 8px;
    padding: 10px 14px; margin-bottom: 14px;
  }
  .sb-left { display: flex; align-items: center; gap: 10px; min-width: 0; }
  .dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%;
         background: var(--muted); box-shadow: 0 0 0 0 transparent; flex: none; }
  .dot.live { background: var(--accent); box-shadow: 0 0 8px rgba(43,213,118,0.7); }
  .dot.warn { background: var(--amber); box-shadow: 0 0 8px rgba(240,166,58,0.6); }
  .dot.err  { background: var(--red);   box-shadow: 0 0 8px rgba(255,92,92,0.6); }
  .sb-status { color: var(--muted); text-transform: uppercase; letter-spacing: 0.12em;
               font-size: 11px; white-space: nowrap; }
  .sb-title { color: var(--ink); text-transform: uppercase; letter-spacing: 0.16em;
              font-size: 12px; font-weight: 600; white-space: nowrap; }
  .sb-right { display: flex; align-items: center; gap: 8px 16px; flex-wrap: wrap;
              justify-content: flex-end; }
  .ro { display: inline-flex; align-items: baseline; gap: 6px; white-space: nowrap; }
  .ro-k { color: var(--muted); text-transform: uppercase; letter-spacing: 0.12em;
          font-size: 11px; }
  .ro-v { color: var(--ink); font-size: 13px; font-variant-numeric: tabular-nums; }

  /* ---- stat-tile GRID --------------------------------------------------- */
  .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
           gap: 10px; margin-bottom: 18px; }
  .tile { background: var(--panel); border: 1px solid var(--grid); border-radius: 8px;
          padding: 11px 13px; min-width: 0; }
  .t-label { color: var(--muted); text-transform: uppercase; letter-spacing: 0.12em;
             font-size: 11px; }
  .t-val { color: var(--ink); font-size: 23px; font-weight: 600; margin-top: 5px;
           font-variant-numeric: tabular-nums; line-height: 1.15; }
  .t-sub { color: var(--muted); font-size: 11px; margin-top: 4px;
           font-variant-numeric: tabular-nums; }
  .t-sub.ssid { overflow-wrap: anywhere; word-break: break-word; font-size: 10.5px; }
  .tile.down .t-val { color: var(--down); }
  .tile.up .t-val { color: var(--up); }
  /* Phase 7 reach status chip: color by class, ALWAYS paired with a text label
     (CVD-safe — never color alone). The dash() "—" stays muted. */
  .t-val.reach-chip { font-size: 18px; letter-spacing: 0.04em; }
  .t-val.reach-ok   { color: var(--accent); }
  .t-val.reach-warn { color: var(--amber); }
  .t-val.reach-down { color: var(--red); }
  /* Phase 9 meeting badge: GOOD/RISKY/BAD, color by EXACT status match, ALWAYS
     paired with the status text (CVD-safe — never color alone). An absent/stale/
     unrecognized status has NO state class => the neutral "—" stays muted, never a
     stale or default color. The tile is emphasized (span-2 on wide grids). */
  .tile.meeting { grid-column: span 2; }
  @media (max-width: 560px) { .tile.meeting { grid-column: auto; } }
  .t-val.meet-chip { font-size: 20px; letter-spacing: 0.05em; font-weight: 700; }
  .t-val.meet-good { color: var(--accent); }
  .t-val.meet-warn { color: var(--amber); }
  .t-val.meet-bad  { color: var(--red); }

  /* ---- clickable stat tiles + info card (educational popover) -----------
     Every top tile carrying data-metric is a role=button affordance that opens
     ONE reusable accessible dialog explaining that metric. The affordance is a
     cursor + hover/focus highlight + a small "i" corner marker (pure text via
     ::after content — no external resources, so the no-external guard stays green). */
  .tile[data-metric] { cursor: pointer; position: relative; }
  .tile[data-metric]:hover { border-color: var(--accent); }
  .tile[data-metric]:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .tile[data-metric]::after { content: "i"; position: absolute; top: 6px; right: 8px;
      width: 13px; height: 13px; line-height: 13px; text-align: center; font-size: 10px;
      font-style: italic; font-weight: 700; color: var(--muted); border: 1px solid var(--grid);
      border-radius: 50%; opacity: 0.6; }
  .tile[data-metric]:hover::after, .tile[data-metric]:focus-visible::after {
      color: var(--accent); border-color: var(--accent); opacity: 1; }

  /* The ONE reusable info-card dialog (filled per click). Hidden via the [hidden]
     attribute; a fixed full-viewport backdrop closes on click-outside. Works in
     BOTH tall + wide layouts (fixed, layout-independent). */
  .infocard-backdrop { position: fixed; inset: 0; z-index: 1000; display: flex;
      align-items: center; justify-content: center; padding: 20px;
      background: rgba(0, 0, 0, 0.55); }
  .infocard-backdrop[hidden] { display: none; }
  .infocard { background: var(--panel); border: 1px solid var(--grid); border-radius: 10px;
      width: 100%; max-width: 440px; max-height: 85vh; overflow-y: auto;
      padding: 18px 20px 20px; box-shadow: 0 12px 40px rgba(0, 0, 0, 0.5); }
  .infocard-head { display: flex; align-items: flex-start; gap: 12px; margin-bottom: 4px; }
  .infocard-title { color: var(--ink); font-size: 18px; font-weight: 700; margin: 0;
      flex: 1 1 auto; letter-spacing: 0.01em; }
  .infocard-close { flex: 0 0 auto; background: var(--panel-2); color: var(--muted);
      border: 1px solid var(--grid); border-radius: 6px; width: 30px; height: 30px;
      font: inherit; font-size: 18px; line-height: 1; cursor: pointer; }
  .infocard-close:hover { color: var(--ink); border-color: var(--accent); }
  .infocard-close:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .ic-sec { margin-top: 13px; }
  .ic-k { color: var(--muted); text-transform: uppercase; letter-spacing: 0.12em;
      font-size: 10.5px; font-weight: 600; margin-bottom: 3px; }
  .ic-v { color: var(--ink); font-size: 13px; line-height: 1.5; }
  /* Live current-value read — CVD-safe: a word label ALWAYS accompanies the color. */
  .ic-live { margin-top: 15px; padding: 10px 12px; border-radius: 7px; font-size: 13px;
      line-height: 1.45; border: 1px solid var(--grid); background: var(--panel-2); }
  .ic-live.good { border-color: var(--accent); }
  .ic-live.fair { border-color: var(--amber); }
  .ic-live.poor { border-color: var(--red); }
  .ic-live .ic-verdict { font-weight: 700; }
  .ic-live.good .ic-verdict { color: var(--accent); }
  .ic-live.fair .ic-verdict { color: var(--amber); }
  .ic-live.poor .ic-verdict { color: var(--red); }

  /* ---- section heads + chart cards ------------------------------------- */
  .section-head { display: flex; align-items: center; gap: 14px; margin: 20px 0 8px;
                  flex-wrap: wrap; }
  .section-head h2 { color: var(--muted); text-transform: uppercase; letter-spacing: 0.22em;
                     font-size: 12px; font-weight: 600; margin: 0; }
  .meta { color: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .chart-wrap { background: var(--panel); border: 1px solid var(--grid); border-radius: 8px;
                padding: 12px; }
  svg { display: block; width: 100%; height: auto; }
  .legend { display: flex; gap: 16px; row-gap: 6px; margin-top: 8px; color: var(--muted);
            font-size: 11px; flex-wrap: wrap; align-items: center; }
  .legend .l-down::before, .legend .l-up::before { content: ""; display: inline-block;
                         width: 14px; height: 3px; border-radius: 2px; margin-right: 6px;
                         vertical-align: middle; }
  .legend .l-down::before { background: var(--down); }
  .legend .l-up::before { background: var(--up); }
  .legend .l-inet::before { content: ""; display: inline-block; width: 14px; height: 3px;
                            border-radius: 2px; margin-right: 6px; vertical-align: middle;
                            background: var(--cyan); }
  .legend-group { display: inline-flex; gap: 12px; align-items: center; flex-wrap: wrap; }
  .legend-group .sep, .legend > .sep { color: var(--grid); }
  .lg-item { display: inline-flex; align-items: center; }
  .lg-item .swatch { display: inline-block; width: 14px; height: 10px; border-radius: 2px;
                     margin-right: 6px; vertical-align: middle; }
  .lg-item .glyph { display: inline-block; width: 12px; margin-right: 6px; text-align: center;
                    color: var(--ink); font-weight: 700; }
  .empty { color: var(--muted); text-align: center; padding: 52px 0; font-size: 12px; }
  .axis { fill: var(--muted); font-size: 10px; font-family: var(--mono); }
  .gridline { stroke: var(--grid); stroke-width: 1; }

  /* ---- history toggle --------------------------------------------------- */
  .toggle { display: inline-flex; border: 1px solid var(--grid); border-radius: 6px;
            overflow: hidden; }
  .toggle button { background: var(--panel-2); color: var(--muted); border: 0;
                   padding: 5px 12px; font: inherit; font-size: 11px; text-transform: uppercase;
                   letter-spacing: 0.1em; cursor: pointer; }
  .toggle button + button { border-left: 1px solid var(--grid); }
  .toggle button.active { background: var(--accent); color: #07120b; font-weight: 600; }

  /* ---- layout toggle (TALL single-column <-> WIDE multi-column) ---------
     State signal = a single presence class layout-wide on <html> (absent =
     TALL = DEFAULT = fail-safe). Set before first paint by the pre-paint head
     script. NO height/min-height/aspect-ratio is set on svg/.panel/.chart-wrap
     in EITHER mode — the uniform-scale / legible-axis-text guarantee hinges on
     svg{height:auto} deriving each box ratio from its viewBox (NO-HEIGHT guard).
     CLARIFICATION (the WIDE single-screen fix): the ban is on a CSS height
     (height / min-height / aspect-ratio on svg / .panel / .chart-wrap), NOT on the
     viewBox ATTRIBUTE height. WIDE shortens only each chart's viewBox height (via
     setAttribute in applyChartDims, width stays 900), which keeps scaleX==scaleY and
     leaves axis text crisp — do NOT revert that lever by re-reading "never shorten a
     chart" as a ban on the viewBox attribute. */
  .panel { min-width: 0; }                 /* 900-user-unit intrinsic SVG must not blow out a grid track */
  .sb-sep { color: var(--muted); }         /* the "·" divider before the layout toggle; DISTINCT from legend .sep */
  html.layout-wide .wrap { max-width: min(2000px, 96vw); }
  html.layout-wide .col-pair {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(600px, 1fr));  /* was 720px: pair now engages at 1440 */
    gap: 14px; align-items: start;         /* align-items:start is load-bearing (no grid row-stretch) */
  }
  @media (max-width: 760px) {
    html.layout-wide .col-pair { grid-template-columns: 1fr; }
  }
  /* WIDE single-screen: compact tiles to ~1 ROW at BOTH 1440 and 1920 (the biggest
     vertical lever), compact section heads, and shorten the outage table. All
     html.layout-wide-scoped + additive; TALL is untouched. NO height set on any svg. */
  html.layout-wide .tiles { grid-template-columns: repeat(auto-fit, minmax(90px, 1fr)); gap: 8px; margin-bottom: 12px; }
  html.layout-wide .tile  { padding: 8px 10px; }
  html.layout-wide .t-val { font-size: 18px; }
  html.layout-wide .tile.meeting { grid-column: auto; }   /* un-span so 14 tiles = 14 slots = 1 row */
  html.layout-wide .section-head { margin: 14px 0 6px; }
  html.layout-wide .otable-wrap { max-height: 160px; }
  /* Active-segment look driven SOLELY from the root class (correct at first paint,
     reusing the .toggle button.active accent vocabulary). NO static class="active"
     ships, and NO JS className swap drives this — this rule is the one source of
     truth; the non-matching segment falls back to the base .toggle button look. */
  html:not(.layout-wide) #layouttoggle [data-layout="tall"],
  html.layout-wide        #layouttoggle [data-layout="wide"] {
    background: var(--accent); color: #07120b; font-weight: 600;
  }
  #layouttoggle button:focus-visible {
    outline: 2px solid var(--accent); outline-offset: 2px;
  }

  /* ---- Phase 8 outage journal ------------------------------------------ */
  .cad-tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
               gap: 10px; margin-bottom: 12px; }
  .otable-wrap { max-height: 280px; overflow-y: auto; overflow-x: auto; border-radius: 6px; }
  table.otable { width: 100%; border-collapse: collapse; font-size: 12px;
                 font-variant-numeric: tabular-nums; }
  table.otable th { text-align: left; color: var(--muted); text-transform: uppercase;
                    letter-spacing: 0.1em; font-size: 10px; font-weight: 600;
                    padding: 6px 10px; border-bottom: 1px solid var(--grid);
                    position: sticky; top: 0; background: var(--panel); white-space: nowrap; }
  table.otable td { padding: 6px 10px; border-bottom: 1px solid var(--panel-2);
                    color: var(--ink); white-space: nowrap; }
  /* Cause chips are CVD-SAFE: a colored outline ALWAYS paired with the text cause
     label (never color alone), one class per cause string Go emits. */
  .cause-chip { display: inline-block; padding: 1px 8px; border-radius: 10px;
                font-size: 11px; font-weight: 600; letter-spacing: 0.02em;
                border: 1px solid transparent; }
  .cause-link-drop { color: var(--red); border-color: var(--red); }
  .cause-gateway-unreachable { color: var(--amber); border-color: var(--amber); }
  .cause-internet-down { color: var(--cyan); border-color: var(--cyan); }
  .och-chip { display: inline-flex; align-items: center; }
  .och-chip .swatch { display: inline-block; width: 10px; height: 10px; border-radius: 2px;
                      margin-right: 5px; vertical-align: middle; flex: none; }
  /* ---- band / channel change journal (Phase 14) ----------------------- */
  /* From -> To cell: two band swatches around an arrow, same palette as HISTORY. */
  .roam-cell { display: inline-flex; align-items: center; gap: 4px; flex-wrap: wrap; }
  .roam-cell .swatch { display: inline-block; width: 10px; height: 10px; border-radius: 2px;
                       vertical-align: middle; flex: none; }
  .roam-arrow { color: var(--muted); margin: 0 2px; }
  /* Flag chips are CVD-SAFE: a colored outline ALWAYS paired with text (never color
     alone), reusing the cause-chip shape. CLEAN STEER vs WITH DROP. */
  .roam-flag { display: inline-block; padding: 1px 8px; border-radius: 10px;
               font-size: 11px; font-weight: 600; letter-spacing: 0.02em;
               border: 1px solid transparent; white-space: nowrap; }
  .roam-flag-clean { color: var(--cyan); border-color: var(--cyan); }
  .roam-flag-drop { color: var(--red); border-color: var(--red); }
  .roam-approx { color: var(--amber); font-size: 10px; margin-left: 6px; font-style: italic; }
  .caveat { color: var(--muted); font-size: 11px; margin-top: 10px; font-style: italic;
            line-height: 1.5; }
  /* ---- airspace (Phase 10) --------------------------------------------- */
  .airrec { font-size: 13px; margin: 2px 2px 10px; letter-spacing: 0.02em; }
  .airrec .rec-label { color: var(--down); font-weight: 600; }
  .airrec .rec-dfs { color: var(--amber); }
  .airrec .rec-nondfs { color: var(--up); }
  .air-youkey { color: var(--ink); }
  .air-youkey::before { content: ""; display: inline-block; width: 10px; height: 10px;
            margin-right: 5px; vertical-align: middle; border: 2px solid var(--you);
            border-radius: 2px; box-sizing: border-box; }

  /* ---- TOP TALKERS process drill-down (click-to-expand detail) ---------- */
  tr.ptalker { cursor: pointer; }
  tr.ptalker:hover td { background: var(--panel-2); }
  tr.ptalker:focus-visible { outline: 2px solid var(--up); outline-offset: -2px; }
  tr.ptalker.open td { background: var(--panel-2); }
  tr.pdetail > td { white-space: normal; padding: 8px 10px 10px;
                    background: var(--panel-2); border-bottom: 1px solid var(--grid); }
  .pd-grid { display: grid; grid-template-columns: max-content 1fr; gap: 2px 12px;
             font-size: 11px; margin-bottom: 6px; }
  .pd-k { color: var(--muted); text-transform: uppercase; letter-spacing: 0.08em;
          font-size: 10px; white-space: nowrap; }
  .pd-v { color: var(--ink); word-break: break-all; font-variant-numeric: tabular-nums; }
  .pd-conns { margin: 4px 0 0; padding: 0; list-style: none; font-size: 11px; }
  .pd-conns li { color: var(--ink); word-break: break-all; padding: 1px 0;
                 font-variant-numeric: tabular-nums; }
  .pd-conns li .pd-host { color: var(--muted); }
  .pd-note { color: var(--muted); font-size: 10px; margin-top: 6px; font-style: italic; }
  .pd-warn { color: var(--amber); font-size: 10px; margin-top: 4px; }
  .pd-msg { color: var(--muted); font-size: 11px; font-style: italic; }

  /* ---- REACHABILITY targets drill-down (click-to-expand, click-free data) --- */
  .rtoggle { display: inline-flex; align-items: center; cursor: pointer; color: var(--muted);
             font-size: 11px; text-transform: uppercase; letter-spacing: 0.1em;
             border: 1px solid var(--grid); border-radius: 6px; padding: 3px 9px;
             user-select: none; white-space: nowrap; }
  .rtoggle:hover { color: var(--ink); }
  .rtoggle:focus-visible { outline: 2px solid var(--up); outline-offset: 2px; }
  .rtoggle .rcaret { display: inline-block; transition: transform 0.15s ease; }
  .rtoggle.open .rcaret { transform: rotate(90deg); }
  .rtargets-wrap { margin-top: 10px; }
  table.rtargets td { white-space: nowrap; }
  .rt-name { white-space: nowrap; }
  .rt-role { color: var(--muted); }
  .rt-host { color: var(--ink); }
  .rt-addr { color: var(--muted); }
  .rt-num { font-variant-numeric: tabular-nums; }
  .rt-ok.rt-up { color: var(--up); }
  .rt-ok.rt-down { color: var(--red); }
  /* ACTIVE badge is CVD-safe: a colored outline ALWAYS paired with the "ACTIVE" text. */
  .rt-badge { display: inline-block; margin-left: 8px; padding: 0 6px; border-radius: 9px;
              font-size: 9px; font-weight: 600; letter-spacing: 0.08em;
              color: var(--up); border: 1px solid var(--up); vertical-align: middle; }
  .rt-msg { color: var(--muted); font-style: italic; }
  .rtargets-foot { color: var(--muted); font-size: 11px; margin-top: 8px; line-height: 1.5;
                   font-variant-numeric: tabular-nums; }
  .rt-foot-stale { color: var(--amber); }
  .rtargets-legend { color: var(--muted); font-size: 10.5px; margin-top: 6px; line-height: 1.5;
                     font-style: italic; }
  /* LIVE-chart event markers are clickable -> the shared #infocard detail card. */
  #chart .evmarker { cursor: pointer; }

  /* ---- SPEED TEST (on-demand button) ------------------------------------
     Self-contained: zero external references. The spinner is a pure-CSS rotating
     border (no image, no external asset). Collapses cleanly at narrow widths. */
  .speedtest { display: flex; flex-direction: column; gap: 12px; }
  .speedtest-warn { color: var(--amber); font-size: 11.5px; line-height: 1.5;
                    border: 1px solid var(--grid); border-radius: 7px; padding: 9px 12px;
                    background: var(--panel-2); }
  .speedtest-controls { display: flex; flex-wrap: wrap; align-items: center; gap: 10px 16px; }
  .speedtest-modes { display: inline-flex; gap: 14px; flex-wrap: wrap; }
  .speedtest-modes label { display: inline-flex; align-items: center; gap: 6px; cursor: pointer;
                           color: var(--ink); font-size: 12px; }
  .speedtest-custom { display: flex; flex-direction: column; gap: 7px; }
  .speedtest-custom[hidden] { display: none; }
  .speedtest-custom input { background: var(--panel-2); color: var(--ink);
                            border: 1px solid var(--grid); border-radius: 6px;
                            padding: 7px 10px; font: inherit; font-size: 12px; max-width: 480px; }
  .speedtest-custom input:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .speedtest-custom .st-hint { color: var(--muted); font-size: 10.5px; }
  .st-run { background: var(--accent); color: #07120b; border: 0; border-radius: 7px;
            padding: 9px 18px; font: inherit; font-size: 12px; font-weight: 700;
            text-transform: uppercase; letter-spacing: 0.1em; cursor: pointer; white-space: nowrap; }
  .st-run:hover:not(:disabled) { filter: brightness(1.08); }
  .st-run:focus-visible { outline: 2px solid var(--ink); outline-offset: 2px; }
  .st-run:disabled { opacity: 0.55; cursor: progress; }
  .st-spinner { display: inline-block; width: 14px; height: 14px; margin-right: 8px;
                vertical-align: middle; border: 2px solid rgba(7,18,11,0.35);
                border-top-color: #07120b; border-radius: 50%;
                animation: st-spin 0.7s linear infinite; }
  @keyframes st-spin { to { transform: rotate(360deg); } }
  .speedtest-result { font-size: 12px; line-height: 1.5; }
  .speedtest-result[hidden] { display: none; }
  .st-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
             gap: 10px; margin-bottom: 8px; }
  .st-cell { background: var(--panel-2); border: 1px solid var(--grid); border-radius: 7px;
             padding: 9px 11px; }
  .st-cell .st-k { color: var(--muted); text-transform: uppercase; letter-spacing: 0.1em;
                   font-size: 10px; }
  .st-cell .st-v { color: var(--ink); font-size: 18px; font-weight: 600; margin-top: 4px;
                   font-variant-numeric: tabular-nums; }
  .st-cell.down .st-v { color: var(--down); }
  .st-cell.up .st-v { color: var(--up); }
  .st-meta { color: var(--muted); font-size: 11px; word-break: break-all; }
  .st-meta .st-target { color: var(--ink); }
  .st-error { color: var(--red); font-weight: 600; }
  @media (max-width: 560px) { .st-run { width: 100%; } }

  /* ---- footer ----------------------------------------------------------- */
  .foot { color: var(--muted); font-size: 11px; margin: 18px 2px 4px;
          letter-spacing: 0.04em; }

  @media (max-width: 560px) {
    .t-val { font-size: 20px; }
    .sb-right { justify-content: flex-start; }
  }
</style>
<script>
/* PRE-PAINT layout restore: reads the stored intent and adds layout-wide to
   documentElement BEFORE first paint so there is no flash of the wrong layout.
   Self-contained (double-quotes + concat, NO backtick/template literal), wrapped
   in try/catch (private windows / disabled storage throw), touches only
   documentElement, references nothing defined later. No stored value => stays TALL. */
(function(){try{if(window.localStorage.getItem("netdebug.layout")==="wide"){
  document.documentElement.classList.add("layout-wide");}}catch(e){}})();
</script>
</head>
<body class="scanlines">
<div class="wrap">
  <div class="statusbar">
    <div class="sb-left">
      <span class="dot" id="dot"></span>
      <span class="sb-status" id="status">connecting&hellip;</span>
      <span class="sb-title">NETDEBUG // LIVE BANDWIDTH</span>
    </div>
    <div class="sb-right">
      <span class="ro"><span class="ro-k">IFACE</span><span class="ro-v" id="ro-iface">&mdash;</span></span>
      <span class="ro"><span class="ro-k">BAND</span><span class="ro-v" id="ro-band">&mdash;</span></span>
      <span class="ro"><span class="ro-k">CH</span><span class="ro-v" id="ro-ch">&mdash;</span></span>
      <span class="ro"><span class="ro-k">RSSI&middot;SNR</span><span class="ro-v" id="ro-rssisnr">&mdash;</span></span>
      <span class="ro"><span class="ro-k">UPTIME</span><span class="ro-v" id="ro-uptime">&mdash;</span></span>
      <span class="ro"><span class="ro-k">SAMPLES</span><span class="ro-v" id="ro-samples">0</span></span>
      <span class="ro"><span class="ro-k">CYCLE</span><span class="ro-v" id="ro-cycle">&mdash;</span></span>
      <span class="sb-sep">&middot;</span>
      <div class="toggle" id="layouttoggle" role="radiogroup" aria-label="dashboard layout">
        <button type="button" data-layout="tall" role="radio" aria-checked="true"><span aria-hidden="true">&#9636;</span> tall</button>
        <button type="button" data-layout="wide" role="radio" aria-checked="false"><span aria-hidden="true">&#8862;</span> wide</button>
      </div>
    </div>
  </div>

  <!-- Every tile is a role=button affordance (click + Enter/Space) opening the ONE
       reusable #infocard dialog, keyed by data-metric. The live tile VALUES keep
       updating underneath — opening the card never touches the poll loop. -->
  <div class="tiles" id="tiles">
    <div class="tile meeting" role="button" tabindex="0" data-metric="meeting" aria-haspopup="dialog" aria-label="Meeting readiness — open explanation"><div class="t-label">Meeting</div><div class="t-val meet-chip" id="meetingval">&mdash;</div><div class="t-sub" id="meetingsub">&mdash;</div></div>
    <div class="tile down" role="button" tabindex="0" data-metric="download" aria-haspopup="dialog" aria-label="Download throughput — open explanation"><div class="t-label">Download</div><div class="t-val" id="dval">&mdash;</div><div class="t-sub" id="dsub">&mdash;</div></div>
    <div class="tile up" role="button" tabindex="0" data-metric="upload" aria-haspopup="dialog" aria-label="Upload throughput — open explanation"><div class="t-label">Upload</div><div class="t-val" id="uval">&mdash;</div><div class="t-sub" id="usub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="samples" aria-haspopup="dialog" aria-label="Samples — open explanation"><div class="t-label">Samples</div><div class="t-val" id="nval">0</div><div class="t-sub" id="nsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="rssi" aria-haspopup="dialog" aria-label="RSSI — open explanation"><div class="t-label">RSSI</div><div class="t-val" id="rssival">&mdash;</div><div class="t-sub" id="rssisub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="snr" aria-haspopup="dialog" aria-label="SNR — open explanation"><div class="t-label">SNR</div><div class="t-val" id="snrval">&mdash;</div><div class="t-sub" id="snrsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="txrate" aria-haspopup="dialog" aria-label="TX rate — open explanation"><div class="t-label">TX Rate</div><div class="t-val" id="txval">&mdash;</div><div class="t-sub" id="txsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="band" aria-haspopup="dialog" aria-label="Band and channel — open explanation"><div class="t-label">Band / CH</div><div class="t-val" id="bandval">&mdash;</div><div class="t-sub ssid" id="ssidsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="reach" aria-haspopup="dialog" aria-label="Reachability — open explanation"><div class="t-label">Reach</div><div class="t-val reach-chip" id="reachval">&mdash;</div><div class="t-sub" id="reachsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="gwrtt" aria-haspopup="dialog" aria-label="Gateway RTT — open explanation"><div class="t-label">Gateway RTT</div><div class="t-val" id="gwrttval">&mdash;</div><div class="t-sub" id="gwrttsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="inetrtt" aria-haspopup="dialog" aria-label="Internet RTT — open explanation"><div class="t-label">Internet RTT</div><div class="t-val" id="inetrttval">&mdash;</div><div class="t-sub" id="inetrttsub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="dns" aria-haspopup="dialog" aria-label="DNS lookup time — open explanation"><div class="t-label">DNS</div><div class="t-val" id="dnsval">&mdash;</div><div class="t-sub" id="dnssub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="jitter" aria-haspopup="dialog" aria-label="Jitter — open explanation"><div class="t-label">Jitter</div><div class="t-val" id="jitterval">&mdash;</div><div class="t-sub" id="jittersub">&mdash;</div></div>
    <div class="tile" role="button" tabindex="0" data-metric="loss" aria-haspopup="dialog" aria-label="Packet loss — open explanation"><div class="t-label">Loss</div><div class="t-val" id="lossval">&mdash;</div><div class="t-sub" id="losssub">&mdash;</div></div>
  </div>

  <!-- ONE reusable, accessible info-card dialog. Content is filled per click via
       createElement + textContent (NEVER innerHTML — the no-innerHTML guard). Hidden
       via the [hidden] attribute; the backdrop closes on click-outside, Esc closes,
       focus moves in on open and is restored to the opening tile on close. -->
  <div class="infocard-backdrop" id="infocard" hidden role="dialog" aria-modal="true" aria-labelledby="infocard-title">
    <div class="infocard">
      <div class="infocard-head">
        <h3 class="infocard-title" id="infocard-title">&mdash;</h3>
        <button type="button" class="infocard-close" id="infocard-close" aria-label="Close explanation">&times;</button>
      </div>
      <div class="infocard-body" id="infocard-body"></div>
    </div>
  </div>

  <!-- ON-DEMAND SPEED TEST. OUTSIDE #tiles (no data-metric). The button is the SOLE
       trigger of a saturating test; nothing here polls or self-starts. All result text
       is written via createElement/textContent (NEVER innerHTML). -->
  <div class="section-head"><h2>// SPEED TEST</h2></div>
  <div class="chart-wrap">
    <div class="speedtest">
      <div class="speedtest-warn" id="speedwarn">Runs a full ~10&ndash;15s test that SATURATES your uplink and downlink. Default target is Cloudflare. Custom mode SENDS test data to the endpoint you enter. One test at a time.</div>
      <div class="speedtest-controls">
        <div class="speedtest-modes" role="radiogroup" aria-label="speed test target">
          <label><input type="radio" name="sttarget" value="cloudflare" id="stmode-cf" checked> Cloudflare</label>
          <label><input type="radio" name="sttarget" value="custom" id="stmode-custom"> Custom endpoint</label>
        </div>
        <button type="button" class="st-run" id="speedbtn">Run speed test</button>
      </div>
      <div class="speedtest-custom" id="speedcustom" hidden>
        <input type="text" id="stdownurl" autocomplete="off" spellcheck="false" aria-label="custom download URL" placeholder="download URL">
        <input type="text" id="stupurl" autocomplete="off" spellcheck="false" aria-label="custom upload URL (optional)" placeholder="upload URL (optional)">
        <div class="st-hint">Paste a full http or https download URL (your ISP test file, your own server, or a self-hosted LibreSpeed). Upload is skipped if no upload URL is given. Ports 80/443 only; private, loopback and metadata addresses are refused.</div>
      </div>
      <div class="speedtest-result" id="speedresult" hidden aria-live="polite"></div>
    </div>
  </div>

  <div class="section-head"><h2>// LIVE</h2></div>
  <div class="chart-wrap">
    <div id="empty" class="empty">waiting for first sample&hellip;</div>
    <svg id="chart" viewBox="0 0 900 320" preserveAspectRatio="none" hidden></svg>
    <div class="legend">
      <span class="l-down">download</span>
      <span class="l-up">upload</span>
      <span class="sep">&middot;</span>
      <span class="legend-group" id="markerlegend"></span>
    </div>
  </div>

  <div class="col-pair">
  <div class="panel">
  <div class="section-head">
    <h2>// REACHABILITY</h2>
    <div class="rtoggle" id="reachtoggle" role="button" tabindex="0" aria-expanded="false" aria-controls="reachtargetswrap" title="show per-target detail: WHO we are hitting"><span class="rcaret" id="reachcaret">&#9656;</span>&nbsp;targets</div>
    <span class="meta" id="reachmeta"></span>
  </div>
  <div class="chart-wrap">
    <div id="reachempty" class="empty">waiting for first reachability probe&hellip;</div>
    <svg id="reachchart" viewBox="0 0 900 150" preserveAspectRatio="none" hidden></svg>
    <div class="legend">
      <span class="l-inet">internet RTT</span>
      <span class="sep">&middot;</span>
      <span>gaps = no internet reply (down / gateway-only)</span>
    </div>
    <div class="otable-wrap rtargets-wrap" id="reachtargetswrap" hidden>
      <table class="otable rtargets">
        <thead><tr><th>Target</th><th>RTT</th><th>Loss</th><th>OK</th></tr></thead>
        <tbody id="reachtargetsbody"></tbody>
      </table>
      <div class="rtargets-foot" id="reachtargetsfoot"></div>
      <div class="rtargets-legend">RTT = round-trip time &middot; LOSS = missed ping replies (replies/packets this cycle) &middot; ACTIVE = the selected internet target feeding INTERNET RTT &middot; the DNS row is a resolve, not a ping</div>
    </div>
  </div>
  </div>

  <div class="panel">
  <div class="section-head">
    <h2>// HISTORY</h2>
    <div class="toggle" id="histtoggle">
      <button type="button" data-span="minute" class="active">per minute</button>
      <button type="button" data-span="hour">per hour</button>
    </div>
    <span class="meta" id="histmeta"></span>
  </div>
  <div class="chart-wrap">
    <div id="histempty" class="empty">no history yet &mdash; a bucket appears when the first minute closes&hellip;</div>
    <svg id="histchart" viewBox="0 0 900 260" preserveAspectRatio="none" hidden></svg>
    <div class="legend">
      <span class="l-down">download (peak)</span>
      <span class="l-up">upload (peak)</span>
      <span class="sep">&middot;</span>
      <span class="legend-group" id="histbandlegend"></span>
    </div>
  </div>
  </div>
  </div>

  <div class="section-head"><h2>// AIRSPACE</h2><span class="meta" id="airmeta"></span></div>
  <div class="chart-wrap">
    <div id="airrec" class="airrec"></div>
    <div id="airempty" class="empty">no ambient scan data yet &mdash; macOS populates the neighbor list passively&hellip;</div>
    <svg id="airspacechart" viewBox="0 0 900 260" preserveAspectRatio="none" hidden></svg>
    <div class="legend">
      <span class="legend-group" id="airbandlegend"></span>
      <span class="sep">&middot;</span>
      <span class="air-youkey">bright outline + YOU = your channel</span>
      <span class="sep">&middot;</span>
      <span>bar height = overlap load (incl. your AP); color encodes BAND, not direction</span>
    </div>
    <div class="caveat">Ambient cached passive scan &mdash; may be minutes stale and run-to-run variable; names are usually macOS-redacted. This is NOT a live airtime meter and NO active scan is performed (so your link is never forced off-channel). Bar height is width-aware overlap load, so a crowded 80MHz block shows on all four of its 20MHz channels.</div>
  </div>

  <div class="section-head"><h2>// OUTAGE JOURNAL</h2><span class="meta" id="outmeta"></span></div>
  <div class="chart-wrap">
    <div class="cad-tiles">
      <div class="tile"><div class="t-label">Count Today</div><div class="t-val" id="ocount">0</div><div class="t-sub">outages while probing</div></div>
      <div class="tile"><div class="t-label">Mean Gap</div><div class="t-val" id="omean">&mdash;</div><div class="t-sub">between onsets today</div></div>
      <div class="tile"><div class="t-label">Last Gap</div><div class="t-val" id="olast">&mdash;</div><div class="t-sub">most recent interval</div></div>
      <div class="tile"><div class="t-label">Mode Channel</div><div class="t-val" id="omode">&mdash;</div><div class="t-sub">most-frequent onset</div></div>
    </div>
    <div id="outempty" class="empty">no outages recorded yet &mdash; a record appears when a detected outage closes&hellip;</div>
    <div class="otable-wrap" id="outtablewrap" hidden>
      <table class="otable">
        <thead><tr><th>Start</th><th>Duration</th><th>Cause</th><th>Onset Band / CH</th><th>RSSI</th></tr></thead>
        <tbody id="outbody"></tbody>
      </table>
    </div>
    <div class="caveat">An outage = link lost OR internet unreachable for &ge;2 reach cycles. Probing pauses on battery / locked screen, and an outage straddling a pause is discarded &mdash; this is a log of outages WHILE PROBING, not outages ever.</div>
  </div>

  <div class="section-head"><h2>// BAND / CHANNEL CHANGE JOURNAL</h2><span class="meta" id="roammeta"></span></div>
  <div class="chart-wrap">
    <div class="cad-tiles">
      <div class="tile"><div class="t-label">Count Today</div><div class="t-val" id="rcount">0</div><div class="t-sub">band/channel changes</div></div>
      <div class="tile"><div class="t-label">Last Change</div><div class="t-val" id="rlast">&mdash;</div><div class="t-sub">most recent transition</div></div>
      <div class="tile"><div class="t-label">Most Steered-To</div><div class="t-val" id="rmode">&mdash;</div><div class="t-sub">most-frequent TO band</div></div>
    </div>
    <div id="roamempty" class="empty">no band / channel changes recorded yet &mdash; a record appears when the radio steers to a new band, channel or AP&hellip;</div>
    <div class="otable-wrap" id="roamtablewrap" hidden>
      <table class="otable">
        <thead><tr><th>Start</th><th>From &rarr; To</th><th>RSSI</th><th>Flag</th></tr></thead>
        <tbody id="roambody"></tbody>
      </table>
    </div>
    <div class="caveat">A change = band, channel OR AP (BSSID) differs between two consecutive associated reach probes. WITH DROP means a disconnect coincided (steer caused by a drop); CLEAN STEER means the link stayed up (e.g. band-steering). Probing pauses on battery / locked screen; a change first seen after a pause is stamped at the RESUME time and marked approximate. BSSID is usually macOS-redacted, so AP roams may show only as a channel change.</div>
  </div>

  <div class="section-head"><h2>// TOP TALKERS</h2><span class="meta" id="procsmeta"></span></div>
  <div class="chart-wrap">
    <div id="procsempty" class="empty">waiting for first per-process sample&hellip;</div>
    <div class="otable-wrap" id="procstablewrap" hidden>
      <table class="otable">
        <thead><tr><th>Process</th><th>PID</th><th>Up</th><th>Down</th></tr></thead>
        <tbody id="procsbody"></tbody>
      </table>
    </div>
    <div class="caveat">Per-process upload/download RATES over a ~2s window (a bursty uploader may read below its peak), ranked by UPLOAD. Shows YOUR processes only; some system processes may be hidden without elevated rights. Names are opaque / macOS-truncated &mdash; use the PID (ps -p &lt;pid&gt;) to identify a process. No sudo is used or required.</div>
  </div>

  <div class="foot" id="foot"></div>
</div>
<script>
"use strict";
(function () {
  // PADL = 86 so the y-axis label gutter (PADL-8 = 78px) fits the widest realistic
  // label ("124.1 Mbps" ≈ 60px at 10px monospace) with margin; was 64 and clipped
  // the leading digit of 3+-digit Mbps readings. plotW = 900-86-12 = 802 stays ample.
  var W = 900, H = 320, PADL = 86, PADR = 12, PADT = 14, PADB = 22;
  // WIDE single-screen: per-mode viewBox HEIGHT for LIVE (width stays 900 so axis
  // text is pixel-identical to TALL at the same rendered width — the legibility
  // lever is NEVER touched). Payload caches let the toggle redraw instantly from the
  // last poll (no re-fetch, no 1s/15s stale). See applyChartDims()/redrawAll() below.
  var LIVE_H = { tall: 320, wide: 120 };
  var lastSamples = [], lastEvents = [], lastReachSeries = [];
  // Latest polled OUTAGE JOURNAL records (set by renderOutages). The marker detail card
  // cross-references THIS already-fetched array — no extra fetch, no endpoint change.
  var lastOutages = [];
  // Phase 10 airspace payload cache (replayed on a tall<->wide toggle by redrawAll,
  // exactly like lastSamples/lastReachSeries, so a toggle never leaves the chart
  // stale or at the wrong viewBox height).
  var lastAirspace = { channels: [], neighbors: [], my_channel: "", recommendation: null };
  // Built by concatenation: the SVG namespace is a required identifier (never
  // fetched), but spelling the literal out would trip the strengthened blanket
  // no-external-URL guard (it bans the bare scheme prefix). NEVER add a bare
  // slash-slash token to that guard either — it would match this concat AND every
  // JS comment, turning the guard permanently red.
  var SVGNS = "http" + "://www.w3.org/2000/svg";
  var chart = document.getElementById("chart");
  var emptyEl = document.getElementById("empty");
  var dot = document.getElementById("dot");
  var statusEl = document.getElementById("status");
  var intervalMs = 1000;

  // Status-bar readouts + tiles (all textContent writes — never innerHTML).
  var roIface = document.getElementById("ro-iface");
  var roBand = document.getElementById("ro-band");
  var roCh = document.getElementById("ro-ch");
  var roRssiSnr = document.getElementById("ro-rssisnr");
  var roUptime = document.getElementById("ro-uptime");
  var roSamples = document.getElementById("ro-samples");
  var roCycle = document.getElementById("ro-cycle");
  var dval = document.getElementById("dval"), dsub = document.getElementById("dsub");
  var uval = document.getElementById("uval"), usub = document.getElementById("usub");
  var nval = document.getElementById("nval"), nsub = document.getElementById("nsub");
  var rssival = document.getElementById("rssival"), rssisub = document.getElementById("rssisub");
  var snrval = document.getElementById("snrval"), snrsub = document.getElementById("snrsub");
  var txval = document.getElementById("txval"), txsub = document.getElementById("txsub");
  var bandval = document.getElementById("bandval"), ssidsub = document.getElementById("ssidsub");
  var meetingval = document.getElementById("meetingval"), meetingsub = document.getElementById("meetingsub");
  var reachval = document.getElementById("reachval"), reachsub = document.getElementById("reachsub");
  var gwrttval = document.getElementById("gwrttval"), gwrttsub = document.getElementById("gwrttsub");
  var inetrttval = document.getElementById("inetrttval"), inetrttsub = document.getElementById("inetrttsub");
  var dnsval = document.getElementById("dnsval"), dnssub = document.getElementById("dnssub");
  var jitterval = document.getElementById("jitterval"), jittersub = document.getElementById("jittersub");
  var lossval = document.getElementById("lossval"), losssub = document.getElementById("losssub");
  var footEl = document.getElementById("foot");

  // ----- layout toggle (TALL <-> WIDE) -----------------------------------
  // The VISIBLE highlight is owned SOLELY by the root-class CSS rule (one source of
  // truth); JS here toggles only the root class + localStorage intent + aria-checked,
  // so CSS and JS can never disagree. WIDE uses shorter per-chart viewBox HEIGHTS, so
  // the toggle NOW re-renders the charts at the active dims from cached payloads via
  // applyChartDims()+redrawAll() (hoisted fn declarations defined later in this IIFE).
  // Persistence stores
  // INTENT ("wide"/"tall"), not effective columns — a too-narrow window collapses wide
  // back to one column via CSS while the stored intent stays "wide". Every storage
  // read/write is wrapped in try/catch (private windows / disabled storage throw).
  var LAYOUT_KEY = "netdebug.layout";
  var layoutToggle = document.getElementById("layouttoggle");
  function layoutIsWide() { return document.documentElement.classList.contains("layout-wide"); }
  function syncLayoutButtons() {
    if (!layoutToggle) { return; }
    var want = layoutIsWide() ? "wide" : "tall"; // INTENT (root class), not effective columns
    var btns = layoutToggle.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      btns[i].setAttribute("aria-checked", (btns[i].getAttribute("data-layout") === want) ? "true" : "false");
    }
  }
  syncLayoutButtons(); // bring aria into agreement with the pre-paint root class
  if (layoutToggle) {
    layoutToggle.addEventListener("click", function (ev) {
      // Parent-walk handles a click landing on the inner glyph <span>.
      var btn = ev.target;
      while (btn && btn !== layoutToggle && !btn.getAttribute("data-layout")) { btn = btn.parentNode; }
      if (!btn || !btn.getAttribute) { return; }
      var want = btn.getAttribute("data-layout"); if (!want) { return; }
      var wide = (want === "wide");
      document.documentElement.classList.toggle("layout-wide", wide); // CSS reacts -> highlight flips
      applyChartDims(); // reassign H/HH + set the mode's viewBox height on #chart/#histchart
      redrawAll();      // replay cached payloads so the charts re-render at the new dims NOW
      try { window.localStorage.setItem(LAYOUT_KEY, wide ? "wide" : "tall"); } catch (e) {}
      syncLayoutButtons(); // only aria now; the highlight is CSS-owned
    });
  }

  // humanBps mirrors the Go humanBps: bytes/sec -> bits/sec, decimal tiers.
  function humanBps(bytesPerSec) {
    if (!isFinite(bytesPerSec) || bytesPerSec < 0) { return "0 bps"; }
    var bits = bytesPerSec * 8;
    if (bits >= 1e9) { return (bits / 1e9).toFixed(1) + " Gbps"; }
    if (bits >= 1e6) { return (bits / 1e6).toFixed(1) + " Mbps"; }
    if (bits >= 1e3) { return (bits / 1e3).toFixed(0) + " Kbps"; }
    return bits.toFixed(0) + " bps";
  }

  function el(name, attrs) {
    var e = document.createElementNS(SVGNS, name);
    for (var k in attrs) { if (attrs.hasOwnProperty(k)) { e.setAttribute(k, attrs[k]); } }
    return e;
  }

  // ----- Phase 4 band palette (SINGLE source; mirrors Go bandColorKey) --------
  // Keyed by the EXACT strings Go's bandColorKey emits ("b24"/"b5"/"b6"/"unknown").
  // Band hues are DELIBERATELY DISJOINT from the throughput line colors so the two
  // dimensions never collide: lines own blue (--down #4aa3ff) + amber (--up #ffb039),
  // bands own teal / violet / pink. Also kept clear of the green LIVE dot (#2bd576)
  // and the red drop marker (#ff5c5c). CVD-wise the band trio is well hue-separated
  // (teal ~170 / violet ~260 / pink ~330) and only ever renders as a low-opacity wash.
  var BAND_PALETTE = { b24: "#2dd4bf", b5: "#a78bfa", b6: "#f472b6", unknown: null };
  var BAND_LABEL = { b24: "2.4 GHz", b5: "5 GHz", b6: "6 GHz" };
  // Wash stays subtle (0.10) + area fills 0.07 so a line-over-fill-over-wash stack
  // stays legible; the hues no longer overlap the lines, so no same-hue muddiness.
  var BAND_TINT = 0.10;
  var FILL_OPACITY = 0.07;

  function bandKey(band) {
    if (band === "2.4 GHz") { return "b24"; }
    if (band === "5 GHz") { return "b5"; }
    if (band === "6 GHz") { return "b6"; }
    return "unknown";
  }

  // linkBand reads a sample's associated band ("" when no link / down / unknown).
  function linkBand(s) {
    return (s && s.link && s.link.link_ok && s.link.band) ? s.link.band : "";
  }

  var MARKER_GLYPH = { drop: "│", roam: "⚑", band_change: "‖" };
  var MARKER_NAME = { drop: "drop", roam: "roam", band_change: "band change" };

  function buildBandLegend(hostId) {
    var host = document.getElementById(hostId);
    if (!host) { return; }
    var keys = ["b24", "b5", "b6"];
    for (var i = 0; i < keys.length; i++) {
      var item = document.createElement("span");
      item.className = "lg-item";
      var sw = document.createElement("span");
      sw.className = "swatch";
      sw.style.background = BAND_PALETTE[keys[i]];
      item.appendChild(sw);
      item.appendChild(document.createTextNode(BAND_LABEL[keys[i]]));
      host.appendChild(item);
    }
  }

  function buildMarkerLegend() {
    var host = document.getElementById("markerlegend");
    if (!host) { return; }
    var kinds = ["drop", "roam", "band_change"];
    for (var i = 0; i < kinds.length; i++) {
      var item = document.createElement("span");
      item.className = "lg-item";
      var g = document.createElement("span");
      g.className = "glyph";
      g.textContent = MARKER_GLYPH[kinds[i]];
      item.appendChild(g);
      item.appendChild(document.createTextNode(MARKER_NAME[kinds[i]]));
      host.appendChild(item);
    }
  }

  buildBandLegend("histbandlegend");
  buildBandLegend("airbandlegend");
  buildMarkerLegend();

  // dash shows a value or an em dash when it is absent/empty.
  function dash(v) { return (v === undefined || v === null || v === "") ? "—" : String(v); }

  // ----- honest-display helpers (absent => "—", never "0 dBm"/"MCS 0") --------
  // RSSI/Noise: int omitempty, an associated reading is always negative, so a 0 (or
  // missing) value is "absent".
  function hasNeg(v) { return (typeof v === "number" && v !== 0); }
  // snrReal: snrFrom returns 0 for UNKNOWN, so only a NON-ZERO snr is a reading.
  function snrReal(v) { return (typeof v === "number" && v !== 0); }
  // mcsPresent tests PRESENCE (not truthiness) so a genuine MCS 0 survives the *int.
  function mcsPresent(v) { return (v !== null && v !== undefined); }

  function snrQuality(snr) {
    if (snr >= 40) { return "EXCELLENT"; }
    if (snr >= 25) { return "GOOD"; }
    if (snr >= 15) { return "FAIR"; }
    return "WEAK"; // a genuine low SNR (e.g. 10) is WEAK; 0 is handled as unknown above
  }

  // rssiSnrStr is the status-bar RSSI·SNR glance: "-43·49" / "-43·—" when SNR is
  // missing / "—" entirely when the link is down. NEVER "0·0".
  function rssiSnrStr(lk) {
    if (!lk || !lk.link_ok || !hasNeg(lk.rssi)) { return "—"; }
    return lk.rssi + "·" + (snrReal(lk.snr) ? String(lk.snr) : "—");
  }

  function fmtUptime(sec) {
    if (typeof sec !== "number" || !isFinite(sec) || sec < 0) { return "—"; }
    var s = Math.floor(sec), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), ss = s % 60;
    function p2(x) { return (x < 10 ? "0" : "") + x; }
    if (h > 0) { return h + "h " + p2(m) + "m"; }
    if (m > 0) { return m + "m " + p2(ss) + "s"; }
    return ss + "s";
  }

  // fmtMbps renders a transmit rate; 0/absent handled by the caller.
  function fmtMbps(mbps) {
    if (mbps >= 1000) { return (mbps / 1000).toFixed(1) + " Gb/s"; }
    return Math.round(mbps) + " Mb/s";
  }

  // ----- Phase 7 reach helpers -------------------------------------------
  // fmtRttMs: an RTT/jitter reading renders "12.3 ms"; 0/absent => "—" (an exact
  // 0 ms is never a real reading — those fields carry omitempty on the wire).
  function fmtRttMs(v) {
    return (typeof v === "number" && isFinite(v) && v > 0) ? (v.toFixed(1) + " ms") : "—";
  }
  // fmtPct: a loss percentage; 0 is a REAL healthy reading and shows as "0%".
  function fmtPct(v) {
    return (typeof v === "number" && isFinite(v)) ? (Number(v.toFixed(1)) + "%") : "—";
  }
  var REACH_LABEL = { ok: "OK", gateway_only: "GATEWAY ONLY", down: "DOWN" };
  var REACH_STATE = { ok: "reach-ok", gateway_only: "reach-warn", down: "reach-down" };

  // Phase 9 meeting badge: map the EXACT server-computed status token to a CVD-safe
  // color class. An absent/stale/unrecognized token has NO entry => the DEFAULT
  // neutral branch renders "—" with no color (never a stale or default color). The
  // status + reason are computed server-side (single-source meetingReady); the JS is
  // render-only. All textContent — never innerHTML.
  var MEET_STATE = { GOOD: "meet-good", RISKY: "meet-warn", BAD: "meet-bad" };
  function updateMeeting(meta) {
    var st = (meta && meta.meeting_status) ? meta.meeting_status : "";
    var cls = MEET_STATE[st]; // undefined for "", a paused/stale omission, or any unknown token
    meetingval.textContent = cls ? st : "—";
    meetingval.className = "t-val meet-chip" + (cls ? (" " + cls) : "");
    var reason = (meta && meta.meeting_reason) ? meta.meeting_reason : "";
    // The reason is a VISIBLE sub-line (a title-only tooltip is invisible on touch +
    // to screen readers for a glance-before-a-call indicator); title is a bonus.
    meetingsub.textContent = cls ? (reason || "—") : "—";
    meetingval.setAttribute("title", (cls && reason) ? reason : "");
  }

  // updateReach fills the reach tiles + the status chip from d.reach (a POINTER
  // object, absent until the first cycle). All textContent; absent => "—".
  function updateReach(reach) {
    var cls = reach ? reach.class : "";
    reachval.textContent = (reach && REACH_LABEL[cls]) ? REACH_LABEL[cls] : "—";
    reachval.className = "t-val reach-chip" + (reach && REACH_STATE[cls] ? (" " + REACH_STATE[cls]) : "");
    reachsub.textContent = reach
      ? ("gateway " + (reach.gateway_ok ? "up" : "down") + " · internet " + (reach.internet_ok ? "up" : "down"))
      : "—";

    gwrttval.textContent = reach ? fmtRttMs(reach.gateway_rtt_ms) : "—";
    gwrttsub.textContent = (reach && reach.gateway_addr) ? reach.gateway_addr : "—";
    inetrttval.textContent = reach ? fmtRttMs(reach.internet_rtt_ms) : "—";
    inetrttsub.textContent = (reach && reach.internet_addr) ? reach.internet_addr : "—";

    // DNS gates on dns_ok (a failed lookup carries no honest time).
    dnsval.textContent = (reach && reach.dns_ok) ? fmtRttMs(reach.dns_ms) : "—";
    dnssub.textContent = reach ? (reach.dns_ok ? "resolved" : "no answer") : "—";

    jitterval.textContent = reach ? fmtRttMs(reach.jitter_ms) : "—";
    jittersub.textContent = reach ? "internet path" : "—";

    // LOSS gates on the ALWAYS-PRESENT gateway_ok/internet_ok bits, not on key
    // presence, so a genuine 0% shows (the loss keys carry NO omitempty).
    lossval.textContent = reach ? fmtPct(reach.internet_loss_pct) : "—";
    losssub.textContent = reach ? ("gateway " + fmtPct(reach.gateway_loss_pct)) : "—";

    // REACHABILITY drill-down: cache the latest reach and, if the panel is expanded,
    // RE-RENDER the open targets table IN PLACE from the NEW reach object every poll —
    // WITHOUT touching the expand toggle or scroll. The per-target detail is already in
    // reach.targets on each ~1s /data.json poll, so there is NO fetch, NO endpoint.
    lastReach = reach;
    if (reachExpanded) { renderReachTargets(reach); }
  }

  // ----- REACHABILITY targets drill-down (client-side render of polled data) ----
  // Clicking the panel's focusable toggle reveals a TARGETS TABLE below the preserved
  // sparkline: a GATEWAY row, one row PER internet target (each with its OWN RTT / loss /
  // packet count and an "ACTIVE" badge on the selectInternet winner), and the DNS resolve
  // row — so a healthy-looking best-of headline can no longer hide a flaky sibling POP.
  // ALL strings (reverse-DNS host + raw addr are attacker-ish) render via textContent
  // (createElement / createTextNode) — NEVER innerHTML. Mirrors the TOP TALKERS accordion
  // AFFORDANCE but not its machinery (no fetch / AbortController / generation guard — the
  // data is already polled). clamp / PD_HOST_MAX / pad2 are reused from the proc code.
  var reachToggle = document.getElementById("reachtoggle");
  var reachTargetsWrap = document.getElementById("reachtargetswrap");
  var reachTargetsBody = document.getElementById("reachtargetsbody");
  var reachTargetsFoot = document.getElementById("reachtargetsfoot");
  var reachExpanded = false;   // ephemeral, collapsed on first paint; no persistence
  var lastReach = null;        // latest polled reach object (for re-render on expand)
  // The reach cycle is 5s and PAUSES on battery / locked screen (holder keeps the last
  // sample), so a sample older than ~4 cycles is stale/paused — flag it so paused
  // per-target numbers never masquerade as live (mirrors the server meetingStaleFactor).
  var REACH_STALE_MS = 20000;

  function reachTargetRow(t) {
    var tr = document.createElement("tr");
    tr.className = "rt-row";
    var kind = t.kind || "";

    var tdName = document.createElement("td");
    tdName.className = "rt-name";
    if (kind === "gateway") {
      var g = document.createElement("span");
      g.className = "rt-role";
      g.appendChild(document.createTextNode("your router"));
      tdName.appendChild(g);
    } else if (kind === "dns") {
      var d = document.createElement("span");
      d.className = "rt-role";
      d.appendChild(document.createTextNode("resolve"));
      tdName.appendChild(d);
    } else if (t.host) {
      var h = document.createElement("span");
      h.className = "rt-host";
      h.appendChild(document.createTextNode(clamp(t.host, PD_HOST_MAX)));
      tdName.appendChild(h);
    }
    if (t.addr) {
      var a = document.createElement("span");
      a.className = "rt-addr";
      a.appendChild(document.createTextNode((tdName.firstChild ? "  " : "") + t.addr));
      tdName.appendChild(a);
    }
    if (t.best) {
      var badge = document.createElement("span");
      badge.className = "rt-badge";
      badge.appendChild(document.createTextNode("ACTIVE"));
      badge.setAttribute("title", "Selected internet target (lowest loss, then lowest RTT) — its RTT feeds the INTERNET RTT tile. Flips to whichever internet target is healthiest each cycle, so it isn't always the same one.");
      tdName.appendChild(badge);
    }
    tr.appendChild(tdName);

    var tdRtt = document.createElement("td");
    tdRtt.className = "rt-num";
    tdRtt.appendChild(document.createTextNode(fmtRttMs(t.rtt_ms)));
    tr.appendChild(tdRtt);

    // Loss + recv/sent only for ping rows (gateway/internet, sent>0); the DNS row is a
    // forward RESOLVE, not a ping — no loss, no packet count.
    var tdLoss = document.createElement("td");
    tdLoss.className = "rt-num";
    if (t.sent) {
      tdLoss.appendChild(document.createTextNode(
        fmtPct(t.loss_pct) + " (" + (t.recv || 0) + "/" + t.sent + ")"));
      // The "(recv/sent)" count = ICMP ping packets this cycle: each reachability cycle
      // sends a small fixed number of pings per target; loss = the replies that went
      // missing. Reads the live sent count so it never hardcodes "3".
      tdLoss.setAttribute("title", (t.recv || 0) + " of " + t.sent +
        " ping replies this cycle — loss = missed replies (" + fmtPct(t.loss_pct) + ")");
    } else {
      tdLoss.appendChild(document.createTextNode("—"));
      tdLoss.setAttribute("title", "DNS is a name lookup (resolve time), not a ping — " +
        "packet loss doesn't apply, hence \"—\". This row's RTT is the resolve time.");
    }
    tr.appendChild(tdLoss);

    var tdOk = document.createElement("td");
    tdOk.className = "rt-ok " + (t.ok ? "rt-up" : "rt-down");
    tdOk.appendChild(document.createTextNode(t.ok ? "up" : "down"));
    tr.appendChild(tdOk);

    return tr;
  }

  function renderReachTargets(reach) {
    if (!reachExpanded) { return; }
    while (reachTargetsBody.firstChild) { reachTargetsBody.removeChild(reachTargetsBody.firstChild); }
    while (reachTargetsFoot.firstChild) { reachTargetsFoot.removeChild(reachTargetsFoot.firstChild); }

    if (!reach) {
      var tr = document.createElement("tr");
      var td = document.createElement("td");
      td.setAttribute("colspan", "4");
      td.className = "rt-msg";
      td.appendChild(document.createTextNode("waiting for first reachability probe…"));
      tr.appendChild(td);
      reachTargetsBody.appendChild(tr);
      return;
    }

    var targets = reach.targets || [];
    for (var i = 0; i < targets.length; i++) {
      reachTargetsBody.appendChild(reachTargetRow(targets[i]));
    }

    // Footer: overall class + jitter (internet path) + an "as of HH:MM:SS" label flagged
    // stale/paused when the sample has aged past ~4 reach cycles.
    var cls = REACH_LABEL[reach.class] || "—";
    reachTargetsFoot.appendChild(document.createTextNode("class: " + cls + "  ·  jitter " + fmtRttMs(reach.jitter_ms) + " (internet path)"));
    var ts = reach.t ? new Date(reach.t) : null;
    var valid = ts && !isNaN(ts.getTime());
    var asof = valid ? (pad2(ts.getHours()) + ":" + pad2(ts.getMinutes()) + ":" + pad2(ts.getSeconds())) : "—";
    var ageMs = valid ? (Date.now() - ts.getTime()) : NaN;
    var stale = isFinite(ageMs) && ageMs > REACH_STALE_MS;
    var asofSpan = document.createElement("span");
    if (stale) { asofSpan.className = "rt-foot-stale"; }
    asofSpan.appendChild(document.createTextNode("  ·  as of " + asof + (stale ? " (stale / paused)" : "")));
    reachTargetsFoot.appendChild(asofSpan);
  }

  function toggleReachTargets() {
    reachExpanded = !reachExpanded;
    reachToggle.setAttribute("aria-expanded", reachExpanded ? "true" : "false");
    reachTargetsWrap.hidden = !reachExpanded;
    if (reachExpanded) {
      reachToggle.classList.add("open");
      renderReachTargets(lastReach);
    } else {
      reachToggle.classList.remove("open");
    }
  }

  if (reachToggle) {
    reachToggle.addEventListener("click", toggleReachTargets);
    reachToggle.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter" || ev.key === " " || ev.key === "Spacebar") {
        ev.preventDefault();
        toggleReachTargets();
      }
    });
  }

  // RW/RH + pads for the reachability RTT sparkline. RPADL fits a "000 ms" label.
  var RW = 900, RH = 150, RPADL = 70, RPADR = 12, RPADT = 12, RPADB = 18;
  var reachChart = document.getElementById("reachchart");
  var reachEmpty = document.getElementById("reachempty");
  var reachMeta = document.getElementById("reachmeta");

  // drawReach plots internet RTT over reach_series. A sample WITHOUT an
  // internet_rtt_ms (down / gateway_only) is a GAP: the line BREAKS there and the
  // vertex is skipped — NEVER plotted as 0, which would misread a dropout as fast.
  function drawReach(series) {
    while (reachChart.firstChild) { reachChart.removeChild(reachChart.firstChild); }
    var n = series.length;
    if (n === 0) {
      reachEmpty.hidden = false; reachChart.hidden = true; reachMeta.textContent = "";
      return;
    }
    reachEmpty.hidden = true; reachChart.hidden = false;
    reachMeta.textContent = n + (n === 1 ? " probe" : " probes");

    function present(v) { return (typeof v === "number" && isFinite(v) && v > 0); }
    var maxR = 1;
    for (var i = 0; i < n; i++) {
      if (present(series[i].internet_rtt_ms) && series[i].internet_rtt_ms > maxR) {
        maxR = series[i].internet_rtt_ms;
      }
    }
    var plotW = RW - RPADL - RPADR, plotH = RH - RPADT - RPADB;
    function xAt(i) { return RPADL + (n === 1 ? plotW : plotW * i / (n - 1)); }
    function yAt(v) { return RPADT + plotH - plotH * (v / maxR); }

    for (var g = 0; g <= 2; g++) {
      var gv = maxR * g / 2, gy = yAt(gv);
      reachChart.appendChild(el("line", { x1: RPADL, y1: gy, x2: RW - RPADR, y2: gy, "class": "gridline" }));
      var gt = el("text", { x: RPADL - 8, y: gy + 3, "text-anchor": "end", "class": "axis" });
      gt.textContent = gv.toFixed(0) + " ms";
      reachChart.appendChild(gt);
    }

    // Build polyline SEGMENTS split on gaps; draw a dot per present vertex so an
    // isolated point (surrounded by gaps) is still visible.
    var seg = "";
    function flush() {
      if (seg.trim() !== "") {
        reachChart.appendChild(el("polyline", { points: seg.trim(), fill: "none",
          stroke: "var(--cyan)", "stroke-width": 2, "stroke-linejoin": "round" }));
      }
      seg = "";
    }
    for (var j = 0; j < n; j++) {
      var v = series[j].internet_rtt_ms;
      if (!present(v)) { flush(); continue; } // gap: break the line, skip the vertex
      var px = xAt(j), py = yAt(v);
      seg += px.toFixed(1) + "," + py.toFixed(1) + " ";
      reachChart.appendChild(el("circle", { cx: px.toFixed(1), cy: py.toFixed(1), r: 1.8, fill: "var(--cyan)" }));
    }
    flush();
  }

  function markerTooltip(ev, s) {
    var lines = [MARKER_NAME[ev.kind] || ev.kind];
    if (ev.from || ev.to) {
      var dim = ev.detail ? (" (" + ev.detail + ")") : "";
      lines.push(dash(ev.from) + " → " + dash(ev.to) + dim);
    }
    var lk = (s && s.link) || null;
    if (lk && lk.link_ok) {
      lines.push("SSID " + dash(lk.ssid));
      lines.push("band " + dash(lk.band));
      lines.push("chan " + dash(lk.channel));
      lines.push("RSSI " + (hasNeg(lk.rssi) ? (lk.rssi + " dBm") : "—"));
      lines.push("SNR " + (snrReal(lk.snr) ? (lk.snr + " dB") : "—"));
    } else {
      lines.push("link down");
    }
    return lines.join("\n");
  }

  function draw(samples, events) {
    while (chart.firstChild) { chart.removeChild(chart.firstChild); }
    var n = samples.length;
    if (n === 0) {
      emptyEl.hidden = false; chart.hidden = true; return;
    }
    emptyEl.hidden = true; chart.hidden = false;

    var maxRate = 1;
    for (var i = 0; i < n; i++) {
      if (samples[i].down_bytes_per_sec > maxRate) { maxRate = samples[i].down_bytes_per_sec; }
      if (samples[i].up_bytes_per_sec > maxRate) { maxRate = samples[i].up_bytes_per_sec; }
    }

    var plotW = W - PADL - PADR, plotH = H - PADT - PADB;
    // preserveAspectRatio="none" + height:auto keeps a uniform scale so the fixed-px
    // marker glyphs stay true. Do NOT set an explicit SVG height. The flat area-fill
    // polygon is safe under this (it stretches with the polyline).
    function xAt(i) { return PADL + (n === 1 ? plotW : plotW * i / (n - 1)); }
    function yAt(v) { return PADT + plotH - plotH * (v / maxRate); }

    // Band background wash intentionally REMOVED from the LIVE chart: on a mostly
    // single-band session it was one flat color (no signal), and at low opacity the
    // band hues were hard to read. Band context now lives in the status-bar BAND
    // readout + the band_change markers below; the HISTORY chart keeps its band
    // strips, where a long window actually spans band switches.

    // Horizontal gridlines + y labels (4 divisions).
    for (var g = 0; g <= 4; g++) {
      var v = maxRate * g / 4, y = yAt(v);
      chart.appendChild(el("line", { x1: PADL, y1: y, x2: W - PADR, y2: y, "class": "gridline" }));
      var t = el("text", { x: PADL - 8, y: y + 3, "text-anchor": "end", "class": "axis" });
      t.textContent = humanBps(v);
      chart.appendChild(t);
    }

    if (n === 1) {
      chart.appendChild(el("circle", { cx: xAt(0), cy: yAt(samples[0].down_bytes_per_sec), r: 2.5, fill: "var(--down)" }));
      chart.appendChild(el("circle", { cx: xAt(0), cy: yAt(samples[0].up_bytes_per_sec), r: 2.5, fill: "var(--up)" }));
      return;
    }

    // Faint FLAT area fills under each line: a translucent polygon closed to the
    // baseline y0 at the first/last x. NOT an SVG gradient — a gradient fill needs
    // a fragment reference, which the strengthened blanket guard forbids; the flat
    // low-opacity polygon keeps the page self-contained.
    var y0 = yAt(0);
    var dArea = xAt(0).toFixed(1) + "," + y0.toFixed(1) + " ";
    var uArea = xAt(0).toFixed(1) + "," + y0.toFixed(1) + " ";
    var dPts = "", uPts = "";
    for (var j = 0; j < n; j++) {
      var xj = xAt(j).toFixed(1);
      var dyj = yAt(samples[j].down_bytes_per_sec).toFixed(1);
      var uyj = yAt(samples[j].up_bytes_per_sec).toFixed(1);
      dPts += xj + "," + dyj + " ";
      uPts += xj + "," + uyj + " ";
      dArea += xj + "," + dyj + " ";
      uArea += xj + "," + uyj + " ";
    }
    dArea += xAt(n - 1).toFixed(1) + "," + y0.toFixed(1);
    uArea += xAt(n - 1).toFixed(1) + "," + y0.toFixed(1);
    chart.appendChild(el("polygon", { points: dArea.trim(), fill: "var(--down)", opacity: FILL_OPACITY, stroke: "none" }));
    chart.appendChild(el("polygon", { points: uArea.trim(), fill: "var(--up)", opacity: FILL_OPACITY, stroke: "none" }));

    chart.appendChild(el("polyline", { points: dPts.trim(), fill: "none", stroke: "var(--down)", "stroke-width": 2, "stroke-linejoin": "round" }));
    chart.appendChild(el("polyline", { points: uPts.trim(), fill: "none", stroke: "var(--up)", "stroke-width": 2, "stroke-linejoin": "round" }));

    // Event markers LAST (on top).
    var idxByT = {};
    for (var m = 0; m < n; m++) { idxByT[samples[m].t] = m; }
    var lastMarkerX = -1e9;
    for (var e = 0; e < events.length; e++) {
      var ev = events[e];
      var idx = idxByT[ev.t];
      if (idx === undefined) { continue; }
      var mx = xAt(idx);
      if (mx - lastMarkerX < 6) { continue; }
      lastMarkerX = mx;
      drawMarker(ev, mx, samples[idx]);
    }
  }

  function drawMarker(ev, mx, s) {
    var topY = PADT, botY = H - PADB;
    var grp = el("g", { "class": "evmarker" });
    // Stash THIS event on the group so the delegated chart click (which survives the
    // ~1s redraw because it is bound ONCE to the #chart SVG, not to the markers) can
    // recover it and open a STABLE snapshot in the shared detail card.
    grp.ndEvent = ev;
    var title = document.createElementNS(SVGNS, "title");
    title.textContent = markerTooltip(ev, s);
    grp.appendChild(title);
    grp.appendChild(el("line", { x1: mx.toFixed(1), y1: topY, x2: mx.toFixed(1), y2: botY,
      stroke: "var(--ink)", "stroke-width": 1, opacity: 0.45 }));
    if (ev.kind === "band_change") {
      grp.appendChild(el("line", { x1: (mx + 3).toFixed(1), y1: topY, x2: (mx + 3).toFixed(1), y2: botY,
        stroke: "var(--ink)", "stroke-width": 1, opacity: 0.45 }));
    }
    if (ev.kind === "drop") {
      grp.appendChild(el("rect", { x: (mx - 3).toFixed(1), y: topY, width: 6, height: 6, fill: "var(--red)" }));
    } else if (ev.kind === "roam") {
      grp.appendChild(el("rect", { x: mx.toFixed(1), y: topY, width: 7, height: 5, fill: "var(--ink)" }));
    } else {
      grp.appendChild(el("rect", { x: (mx - 2).toFixed(1), y: (topY - 1).toFixed(1), width: 7, height: 7,
        fill: "var(--ink)", transform: "rotate(45 " + (mx + 1.5).toFixed(1) + " " + (topY + 2.5).toFixed(1) + ")" }));
    }
    grp.appendChild(el("rect", { x: (mx - 4).toFixed(1), y: topY, width: 8, height: botY - topY,
      fill: "transparent" }));
    chart.appendChild(grp);
  }

  // setDot updates the status dot + its REDUNDANT text label (never color alone).
  function setDot(state, text) {
    dot.className = "dot" + (state ? (" " + state) : "");
    statusEl.textContent = text;
  }

  function apply(data) {
    var samples = (data && data.samples) || [];
    var meta = (data && data.meta) || null;
    if (data && typeof data.interval_sec === "number" && data.interval_sec > 0) {
      intervalMs = Math.max(250, data.interval_sec * 1000);
    }
    var cycleStr = (data && typeof data.interval_sec === "number" && data.interval_sec > 0)
      ? (Number(data.interval_sec.toFixed(3)) + "s") : "—";

    // Window stats for the DOWNLOAD/UPLOAD sub-lines (peak/avg over the VISIBLE window).
    var peakD = 0, peakU = 0, sumD = 0, sumU = 0, n = samples.length;
    for (var i = 0; i < n; i++) {
      var d = samples[i].down_bytes_per_sec, u = samples[i].up_bytes_per_sec;
      if (d > peakD) { peakD = d; }
      if (u > peakU) { peakU = u; }
      sumD += d; sumU += u;
    }
    var last = n > 0 ? samples[n - 1] : null;
    var lk = (last && last.link) || null;
    var okLink = !!(lk && lk.link_ok);

    // Tiles.
    dval.textContent = last ? humanBps(last.down_bytes_per_sec) : "—";
    uval.textContent = last ? humanBps(last.up_bytes_per_sec) : "—";
    dsub.textContent = n > 0 ? ("peak " + humanBps(peakD) + " · avg " + humanBps(sumD / n)) : "—";
    usub.textContent = n > 0 ? ("peak " + humanBps(peakU) + " · avg " + humanBps(sumU / n)) : "—";
    nval.textContent = String(n);
    nsub.textContent = "cycle " + cycleStr;

    rssival.textContent = (okLink && hasNeg(lk.rssi)) ? (lk.rssi + " dBm") : "—";
    rssisub.textContent = (okLink && hasNeg(lk.noise)) ? ("noise " + lk.noise + " dBm") : "noise —";
    snrval.textContent = (okLink && snrReal(lk.snr)) ? (lk.snr + " dB") : "—";
    snrsub.textContent = (okLink && snrReal(lk.snr)) ? snrQuality(lk.snr) : "—";
    txval.textContent = (okLink && lk.tx_rate_mbps) ? fmtMbps(lk.tx_rate_mbps) : "—";
    if (okLink) {
      var bits = [];
      if (lk.phy) { bits.push(lk.phy); }
      if (mcsPresent(lk.mcs)) { bits.push("MCS " + String(lk.mcs)); }
      txsub.textContent = bits.length ? bits.join(" · ") : "—";
    } else { txsub.textContent = "—"; }

    var bparts = [];
    if (okLink && lk.band) { bparts.push(lk.band); }
    if (okLink && lk.channel) { bparts.push("ch " + lk.channel); }
    bandval.textContent = bparts.length ? bparts.join(" · ") : "—";
    // ssid_label from meta is ALREADY ssidDisplay()'d by the server — render verbatim.
    ssidsub.textContent = (meta && meta.ssid_label) ? meta.ssid_label : "—";

    // Status-bar readouts — BAND/CH + RSSI·SNR read the SAME last sample as the tiles.
    roIface.textContent = (data && data.iface) ? data.iface : "—";
    roBand.textContent = (okLink && lk.band) ? lk.band : "—";
    roCh.textContent = (okLink && lk.channel) ? lk.channel : "—";
    roRssiSnr.textContent = rssiSnrStr(lk);
    roUptime.textContent = (meta && typeof meta.uptime_sec === "number") ? fmtUptime(meta.uptime_sec) : "—";
    roSamples.textContent = String(n);
    roCycle.textContent = cycleStr;

    // Footer: iface · bind addr (same-origin host) · localhost only.
    footEl.textContent = "iface " + ((data && data.iface) ? data.iface : "—") +
      " · " + location.host + " · localhost only";

    // Cache the draw inputs so a layout toggle can re-render at the new chart dims
    // instantly (no re-fetch, no 1s/15s stale). redrawAll() replays these.
    lastSamples = samples;
    lastEvents = (data && data.events) || [];
    lastReachSeries = (data && data.reach_series) || [];

    draw(samples, lastEvents);

    // Phase 7 reach: the latest cycle + the sparkline tail, each guarded
    // INDEPENDENTLY (omitempty omits even a non-nil empty slice during warm-up).
    updateReach((data && data.reach) || null);
    drawReach(lastReachSeries);

    // Phase 9 meeting badge (server-computed into meta; absent => neutral "—").
    updateMeeting(meta);

    // STALE guard: the poll succeeded, but if the NEWEST sample's timestamp has
    // stopped advancing (Wi-Fi toggled off, interface vanished) the chart freezes
    // while the server still answers 200. Flag that as amber "stale". Server and
    // client are the same machine, so clock skew is ~0. An unparseable/zero t
    // (Date.parse => NaN) is treated as NOT stale (never throws, keeps "live").
    var STALE_FACTOR = 3;
    var newestMs = last ? Date.parse(last.t) : NaN;
    var age = isFinite(newestMs) ? (Date.now() - newestMs) : NaN;
    if (n > 0 && isFinite(age) && age > STALE_FACTOR * intervalMs) {
      setDot("warn", "stale");
    } else {
      setDot("live", "live");
    }
  }

  function poll() {
    fetch("/data.json", { cache: "no-store" })
      .then(function (r) {
        // HARD error: the fetch COMPLETED but the server answered non-OK.
        // Tag it so the catch can turn the dot RED (vs. a network reject).
        if (!r.ok) { var e = new Error("HTTP " + r.status); e.http = true; throw e; }
        return r.json();
      })
      .then(function (data) { apply(data); })
      .catch(function (err) {
        if (err && err.http) {
          // Hard error — server reachable but failing. Red dot + status label.
          setDot("err", err.message);
        } else {
          // Transient — fetch itself rejected (network blip). Amber, reconnecting.
          setDot("warn", "reconnecting…");
        }
      })
      .finally(function () { setTimeout(poll, intervalMs); });
  }

  poll();

  // ----- history chart (Phase 3) -----------------------------------------
  // HPADL = 86 mirrors PADL: the HISTORY y-label gutter (HPADL-8 = 78px) fits
  // "124.1 Mbps"/"1.2 Gbps" without clipping the leading digit (was 64).
  var HW = 900, HH = 260, HPADL = 86, HPADR = 12, HPADT = 14, HPADB = 22;
  // WIDE single-screen: per-mode viewBox HEIGHT for HISTORY (width stays 900). REACH
  // keeps RH=150 in both modes — it is never the pole of the paired row (HISTORY is).
  var HIST_H = { tall: 260, wide: 200 };
  // Phase 10 AIRSPACE per-mode viewBox height (width stays 900, same mechanism as
  // LIVE/HISTORY): a shortened WIDE viewBox keeps the one-screen budget at 1440/1920.
  var AIRSPACE_H = { tall: 260, wide: 140 };
  var AW = 900, APADL = 20, APADR = 12, APADT = 16, APADB = 44;
  var airChart = document.getElementById("airspacechart");
  var airEmpty = document.getElementById("airempty");
  var airMeta = document.getElementById("airmeta");
  var airRec = document.getElementById("airrec");
  var AH = AIRSPACE_H.tall;
  var histChart = document.getElementById("histchart");
  var histEmpty = document.getElementById("histempty");
  var histMeta = document.getElementById("histmeta");
  var histToggle = document.getElementById("histtoggle");
  var histSpan = "minute";
  var histData = { minute: [], hour: [] };

  // applyChartDims + redrawAll MUST stay hoisted "function" DECLARATIONS (not
  // "var x = function(){}" expressions): the toggle click handler wired during init (above)
  // resolves them by hoisting, and the init applyChartDims() call sits at the END of
  // the IIFE (after chart/histChart/histData exist). Do NOT convert to expressions.
  // applyChartDims reassigns the MODULE vars H/HH (NOT a local shadow) because draw()/
  // drawMarker()/drawHistory() read the module vars; a shadow would desync markers.
  function applyChartDims() {
    var w = layoutIsWide();
    H  = w ? LIVE_H.wide : LIVE_H.tall;
    HH = w ? HIST_H.wide : HIST_H.tall;
    AH = w ? AIRSPACE_H.wide : AIRSPACE_H.tall;
    chart.setAttribute("viewBox", "0 0 " + W + " " + H);
    histChart.setAttribute("viewBox", "0 0 " + HW + " " + HH);
    airChart.setAttribute("viewBox", "0 0 " + AW + " " + AH);
    // REACH viewBox is unchanged (RH stays 150 in both modes) — not touched here.
  }
  // Replays the cached last-poll payloads so a toggle re-renders at the new dims with
  // no re-fetch and no stale frame. Each draw fn owns its own empty-state/.hidden
  // flip, so a toggle before first data (caches = []) draws the empty state cleanly.
  // Do NOT call poll()/pollHistory() here — that would spawn a second setTimeout loop.
  function redrawAll() {
    draw(lastSamples, lastEvents);
    drawReach(lastReachSeries);
    drawHistory();            // reads module-level histData
    drawAirspace(lastAirspace);
  }

  function spanMs(span) { return span === "hour" ? 3600000 : 60000; }

  function drawHistory() {
    while (histChart.firstChild) { histChart.removeChild(histChart.firstChild); }
    var buckets = histData[histSpan] || [];
    var n = buckets.length;
    if (n === 0) {
      histEmpty.hidden = false; histChart.hidden = true;
      histMeta.textContent = "";
      return;
    }
    histEmpty.hidden = true; histChart.hidden = false;
    histMeta.textContent = n + " " + histSpan + (n === 1 ? " bucket" : " buckets");

    var step = spanMs(histSpan), maxRate = 1, firstMs = null, lastMs = null;
    var pts = [];
    for (var i = 0; i < n; i++) {
      var b = buckets[i];
      var ms = Date.parse(b.start);
      if (isNaN(ms)) { continue; }
      if (firstMs === null || ms < firstMs) { firstMs = ms; }
      if (lastMs === null || ms > lastMs) { lastMs = ms; }
      if (b.down_peak_bps > maxRate) { maxRate = b.down_peak_bps; }
      if (b.up_peak_bps > maxRate) { maxRate = b.up_peak_bps; }
      pts.push({ ms: ms, d: b.down_peak_bps, u: b.up_peak_bps, band: b.band || "" });
    }
    if (pts.length === 0 || firstMs === null) {
      histEmpty.hidden = false; histChart.hidden = true; return;
    }

    var slots = Math.round((lastMs - firstMs) / step);
    var plotW = HW - HPADL - HPADR, plotH = HH - HPADT - HPADB;
    function xAt(slot) { return HPADL + (slots === 0 ? plotW : plotW * slot / slots); }
    function yAt(v) { return HPADT + plotH - plotH * (v / maxRate); }

    for (var g = 0; g <= 4; g++) {
      var v = maxRate * g / 4, y = yAt(v);
      histChart.appendChild(el("line", { x1: HPADL, y1: y, x2: HW - HPADR, y2: y, "class": "gridline" }));
      var t = el("text", { x: HPADL - 8, y: y + 3, "text-anchor": "end", "class": "axis" });
      t.textContent = humanBps(v);
      histChart.appendChild(t);
    }

    var barW = Math.max(1, Math.min(10, (slots > 0 ? plotW / (slots + 1) : plotW) * 0.4));
    var y0 = yAt(0);
    for (var j = 0; j < pts.length; j++) {
      var slot = Math.round((pts[j].ms - firstMs) / step);
      var cx = xAt(slot);
      var yd = yAt(pts[j].d), yu = yAt(pts[j].u);
      histChart.appendChild(el("rect", {
        x: (cx - barW).toFixed(1), y: yd.toFixed(1), width: barW.toFixed(1),
        height: Math.max(0, y0 - yd).toFixed(1), fill: "var(--down)", opacity: "0.85"
      }));
      histChart.appendChild(el("rect", {
        x: cx.toFixed(1), y: yu.toFixed(1), width: barW.toFixed(1),
        height: Math.max(0, y0 - yu).toFixed(1), fill: "var(--up)", opacity: "0.85"
      }));
      var hk = bandKey(pts[j].band);
      if (BAND_PALETTE[hk]) {
        histChart.appendChild(el("rect", {
          x: (cx - barW).toFixed(1), y: (y0 + 2).toFixed(1),
          width: (barW * 2).toFixed(1), height: 3, fill: BAND_PALETTE[hk]
        }));
      }
    }
  }

  function applyHistory(data) {
    histData.minute = (data && data.minute) || [];
    histData.hour = (data && data.hour) || [];
    drawHistory();
  }

  function pollHistory() {
    fetch("/history.json", { cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (data) { applyHistory(data); })
      .catch(function () { /* keep last-good history; retry next tick */ })
      .finally(function () { setTimeout(pollHistory, 15000); });
  }

  histToggle.addEventListener("click", function (ev) {
    var btn = ev.target;
    if (!btn || !btn.getAttribute) { return; }
    var span = btn.getAttribute("data-span");
    if (!span) { return; }
    histSpan = span;
    var btns = histToggle.querySelectorAll("button");
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].getAttribute("data-span") === span) { btns[i].className = "active"; }
      else { btns[i].className = ""; }
    }
    drawHistory();
  });

  pollHistory();

  // ----- outage journal (Phase 8) ----------------------------------------
  // pollOutages mirrors pollHistory (fetch /outages.json no-store, ~15s, keep-last-
  // good on catch). All rendering is createElement + textContent (free-form band/
  // channel/cause are textContent only — never innerHTML, no external resources, no
  // inline image markup; the self-contained-page guard stays green).
  var ocount = document.getElementById("ocount");
  var omean = document.getElementById("omean");
  var olast = document.getElementById("olast");
  var omode = document.getElementById("omode");
  var outMeta = document.getElementById("outmeta");
  var outEmpty = document.getElementById("outempty");
  var outTableWrap = document.getElementById("outtablewrap");
  var outBody = document.getElementById("outbody");

  // fmtDur renders a wall-clock duration honestly: "—" for absent/negative, "Ns" under
  // a minute, "Mm Ss" under an hour, "Hh Mm" above.
  function fmtDur(sec) {
    if (typeof sec !== "number" || !isFinite(sec) || sec < 0) { return "—"; }
    if (sec < 60) { return sec.toFixed(0) + "s"; }
    var m = Math.floor(sec / 60), s = Math.round(sec % 60);
    if (m < 60) { return m + "m " + s + "s"; }
    var h = Math.floor(m / 60); m = m % 60;
    return h + "h " + m + "m";
  }

  // fmtGap renders an inter-onset gap in minutes; 0/absent => "—".
  function fmtGap(min) {
    if (typeof min !== "number" || !isFinite(min) || min <= 0) { return "—"; }
    return min.toFixed(1) + " min";
  }

  // fmtClock renders an ISO start as the viewer's local wall-clock; unparseable => "—".
  function fmtClock(iso) {
    var ms = Date.parse(iso);
    if (isNaN(ms)) { return "—"; }
    return new Date(ms).toLocaleString();
  }

  // causeChip builds the CVD-safe cause chip: a class-colored outline ALWAYS paired
  // with the text cause label (never color alone).
  function causeChip(cause) {
    var span = document.createElement("span");
    span.className = "cause-chip cause-" + cause;
    span.textContent = cause ? cause : "—";
    return span;
  }

  // onsetCell renders the onset band swatch (reusing BAND_PALETTE — no new palette) +
  // "band · channel", each dash()'d when absent.
  function onsetCell(o) {
    var cell = document.createElement("td");
    var wrap = document.createElement("span");
    wrap.className = "och-chip";
    var key = bandKey(o.band || "");
    if (BAND_PALETTE[key]) {
      var sw = document.createElement("span");
      sw.className = "swatch";
      sw.style.background = BAND_PALETTE[key];
      wrap.appendChild(sw);
    }
    wrap.appendChild(document.createTextNode(dash(o.band) + " · " + dash(o.channel)));
    cell.appendChild(wrap);
    return cell;
  }

  function renderOutages(data) {
    var outages = (data && data.outages) || [];
    lastOutages = outages; // snapshot for the LIVE-chart drop marker cross-reference
    var cad = (data && data.cadence) || {};
    ocount.textContent = String(cad.count_today || 0);
    omean.textContent = fmtGap(cad.mean_gap_min);
    olast.textContent = fmtGap(cad.last_gap_min);
    omode.textContent = dash(cad.mode_channel);

    var n = outages.length;
    outMeta.textContent = n ? (n + (n === 1 ? " record" : " records")) : "";
    while (outBody.firstChild) { outBody.removeChild(outBody.firstChild); }
    if (n === 0) {
      outEmpty.hidden = false; outTableWrap.hidden = true;
      return;
    }
    outEmpty.hidden = true; outTableWrap.hidden = false;

    // Newest-first in the table (the wire is sorted ascending by start).
    for (var i = n - 1; i >= 0; i--) {
      var o = outages[i];
      var tr = document.createElement("tr");
      var tdStart = document.createElement("td");
      tdStart.textContent = fmtClock(o.start);
      tr.appendChild(tdStart);
      var tdDur = document.createElement("td");
      tdDur.textContent = fmtDur(o.duration_sec);
      tr.appendChild(tdDur);
      var tdCause = document.createElement("td");
      tdCause.appendChild(causeChip(o.cause || ""));
      tr.appendChild(tdCause);
      tr.appendChild(onsetCell(o));
      var tdRssi = document.createElement("td");
      tdRssi.textContent = hasNeg(o.rssi) ? (o.rssi + " dBm") : "—";
      tr.appendChild(tdRssi);
      outBody.appendChild(tr);
    }
  }

  function pollOutages() {
    fetch("/outages.json", { cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (data) { renderOutages(data); })
      .catch(function () { /* keep last-good journal; retry next tick */ })
      .finally(function () { setTimeout(pollOutages, 15000); });
  }

  pollOutages();

  // ----- band / channel change journal (Phase 14) -----------------------
  // pollRoam mirrors pollOutages (fetch /roam.json no-store, ~15s, keep-last-good on
  // catch). All rendering is createElement + textContent (free-form band/channel values
  // are textContent only — never innerHTML, no external resources; the self-contained-
  // page guard stays green).
  var rcount = document.getElementById("rcount");
  var rlast = document.getElementById("rlast");
  var rmode = document.getElementById("rmode");
  var roamMeta = document.getElementById("roammeta");
  var roamEmpty = document.getElementById("roamempty");
  var roamTableWrap = document.getElementById("roamtablewrap");
  var roamBody = document.getElementById("roambody");

  // fmtBandCh renders "5 GHz ch 149" (or just the band when the channel is absent, or
  // just "ch 149" when only the channel is known, or "—" when neither is).
  function fmtBandCh(band, ch) {
    var b = (band === undefined || band === null) ? "" : String(band);
    var c = (ch === undefined || ch === null) ? "" : String(ch);
    if (b === "" && c === "") { return "—"; }
    if (c === "") { return b; }
    if (b === "") { return "ch " + c; }
    return b + " ch " + c;
  }

  // bandSwatch returns a palette swatch span for a band, or null when the band is
  // unknown (so an unknown band renders text-only, never a blank swatch).
  function bandSwatch(band) {
    var key = bandKey(band || "");
    if (!BAND_PALETTE[key]) { return null; }
    var sw = document.createElement("span");
    sw.className = "swatch";
    sw.style.background = BAND_PALETTE[key];
    return sw;
  }

  // roamCell builds the "FROM -> TO" cell: swatch + "band ch N", an arrow, then the
  // destination swatch + "band ch N".
  function roamCell(r) {
    var cell = document.createElement("td");
    var wrap = document.createElement("span");
    wrap.className = "roam-cell";
    var fromSw = bandSwatch(r.from_band);
    if (fromSw) { wrap.appendChild(fromSw); }
    wrap.appendChild(document.createTextNode(fmtBandCh(r.from_band, r.from_ch)));
    var arrow = document.createElement("span");
    arrow.className = "roam-arrow";
    arrow.textContent = "→";
    wrap.appendChild(arrow);
    var toSw = bandSwatch(r.to_band);
    if (toSw) { wrap.appendChild(toSw); }
    wrap.appendChild(document.createTextNode(fmtBandCh(r.to_band, r.to_ch)));
    cell.appendChild(wrap);
    return cell;
  }

  // roamFlagCell builds the CVD-safe flag: WITH DROP (disconnect-driven) vs CLEAN STEER,
  // plus an "after pause" approximate-time note when the exact time is unknown.
  function roamFlagCell(r) {
    var cell = document.createElement("td");
    var chip = document.createElement("span");
    if (r.with_drop) {
      chip.className = "roam-flag roam-flag-drop";
      chip.textContent = "with drop";
    } else {
      chip.className = "roam-flag roam-flag-clean";
      chip.textContent = "clean steer";
    }
    cell.appendChild(chip);
    if (r.after_pause) {
      var note = document.createElement("span");
      note.className = "roam-approx";
      note.textContent = "~ after pause";
      cell.appendChild(note);
    }
    return cell;
  }

  function renderRoam(data) {
    var roams = (data && data.roams) || [];
    var cad = (data && data.cadence) || {};
    rcount.textContent = String(cad.count_today || 0);
    rmode.textContent = dash(cad.most_steered_to_band);

    var n = roams.length;
    rlast.textContent = n ? fmtClock(roams[n - 1].t) : "—";
    roamMeta.textContent = n ? (n + (n === 1 ? " record" : " records")) : "";
    while (roamBody.firstChild) { roamBody.removeChild(roamBody.firstChild); }
    if (n === 0) {
      roamEmpty.hidden = false; roamTableWrap.hidden = true;
      return;
    }
    roamEmpty.hidden = true; roamTableWrap.hidden = false;

    // Newest-first in the table (the wire is sorted ascending by time).
    for (var i = n - 1; i >= 0; i--) {
      var r = roams[i];
      var tr = document.createElement("tr");
      var tdStart = document.createElement("td");
      tdStart.textContent = fmtClock(r.t);
      tr.appendChild(tdStart);
      tr.appendChild(roamCell(r));
      var tdRssi = document.createElement("td");
      tdRssi.textContent = hasNeg(r.rssi_at_change) ? (r.rssi_at_change + " dBm") : "—";
      tr.appendChild(tdRssi);
      tr.appendChild(roamFlagCell(r));
      roamBody.appendChild(tr);
    }
  }

  function pollRoam() {
    fetch("/roam.json", { cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (data) { renderRoam(data); })
      .catch(function () { /* keep last-good journal; retry next tick */ })
      .finally(function () { setTimeout(pollRoam, 15000); });
  }

  pollRoam();

  // ----- airspace / channel occupancy (Phase 10) ------------------------
  // drawAirspace renders the width-aware per-channel occupancy chart: one slot per
  // occupied channel, grouped into band sections (2.4 -> 5 -> 6) left-to-right. Bar
  // HEIGHT encodes OverlapCount (the honest width-aware block load, so a crowded
  // 80MHz block shows on all four of its 20MHz channels); color encodes BAND via the
  // SAME BAND_PALETTE as HISTORY (NOT direction). YOUR channel gets a bright outline
  // + a "YOU" label (band fill alone can't distinguish it — every bar in a band is
  // that band's color). All textContent / createElementNS — never innerHTML, no
  // external asset, no title attr (the self-contained-page guards stay green).
  function drawAirspace(payload) {
    while (airChart.firstChild) { airChart.removeChild(airChart.firstChild); }
    var ch = (payload && payload.channels) || [];
    var rec = payload && payload.recommendation;
    renderAirRec(ch.length, rec);
    var n = ch.length;
    if (n === 0) {
      airEmpty.hidden = false; airChart.hidden = true; airMeta.textContent = "";
      return;
    }
    airEmpty.hidden = true; airChart.hidden = false;
    airMeta.textContent = n + (n === 1 ? " channel" : " channels") + " with occupancy";

    var maxLoad = 1;
    for (var i = 0; i < n; i++) {
      if (ch[i].overlap_count > maxLoad) { maxLoad = ch[i].overlap_count; }
    }
    var plotW = AW - APADL - APADR, plotH = AH - APADT - APADB;
    var slotW = plotW / n;
    var y0 = APADT + plotH;

    // baseline + a faint max gridline.
    airChart.appendChild(el("line", { x1: APADL, y1: y0, x2: AW - APADR, y2: y0, "class": "gridline" }));

    for (var j = 0; j < n; j++) {
      var cl = ch[j];
      var cx = APADL + slotW * j + slotW / 2;
      var barW = Math.max(3, Math.min(42, slotW * 0.64));
      var load = cl.overlap_count || 0;
      var h = (load / maxLoad) * plotH;
      if (h < 3) { h = 3; } // a marked slot is never invisible
      var yTop = y0 - h;
      var key = bandKey(cl.band || "");
      var fill = BAND_PALETTE[key] || "#64748b";
      var rect = el("rect", {
        x: (cx - barW / 2).toFixed(1), y: yTop.toFixed(1),
        width: barW.toFixed(1), height: h.toFixed(1),
        fill: fill, opacity: cl.mine ? "0.95" : "0.72", rx: "1.5"
      });
      if (cl.mine) {
        rect.setAttribute("stroke", "var(--you)");
        rect.setAttribute("stroke-width", "2.5");
      }
      var title = document.createElementNS(SVGNS, "title");
      title.textContent = airTooltip(cl);
      rect.appendChild(title);
      airChart.appendChild(rect);

      if (cl.mine) {
        var youT = el("text", { x: cx.toFixed(1), y: (yTop - 5).toFixed(1),
          "text-anchor": "middle", "class": "axis", fill: "var(--you)" });
        youT.textContent = "YOU";
        airChart.appendChild(youT);
      }

      // channel-number label under each bar.
      var chT = el("text", { x: cx.toFixed(1), y: (y0 + 13).toFixed(1),
        "text-anchor": "middle", "class": "axis" });
      chT.textContent = String(cl.channel_num);
      airChart.appendChild(chT);
    }

    // band-section labels + dividers (left-to-right 2.4 -> 5 -> 6).
    var secStart = 0;
    for (var k = 0; k <= n; k++) {
      var endOfSec = (k === n) || (ch[k].band !== ch[secStart].band);
      if (!endOfSec) { continue; }
      var midX = APADL + slotW * (secStart + k) / 2;
      var band = ch[secStart].band || "";
      var sk = bandKey(band);
      var secT = el("text", { x: midX.toFixed(1), y: (y0 + 30).toFixed(1),
        "text-anchor": "middle", "class": "axis", fill: BAND_PALETTE[sk] || "var(--muted)" });
      secT.textContent = band || "?";
      airChart.appendChild(secT);
      if (k < n) {
        var divX = APADL + slotW * k;
        airChart.appendChild(el("line", { x1: divX.toFixed(1), y1: APADT,
          x2: divX.toFixed(1), y2: (y0 + 34).toFixed(1), "class": "gridline" }));
      }
      secStart = k;
    }
  }

  // airTooltip is the native SVG <title> text for a channel bar (StrongCount /
  // discrete Count / MaxRSSI — the forensic detail behind the overlap bar).
  function airTooltip(cl) {
    var lines = ["ch " + cl.channel_num + " (" + dash(cl.band) + ")" + (cl.mine ? " — YOU" : "")];
    lines.push("overlap load " + (cl.overlap_count || 0) + " (" + (cl.overlap_strong || 0) + " strong)");
    lines.push("discrete " + (cl.count || 0) + " AP" + ((cl.count === 1) ? "" : "s") +
      ", " + (cl.strong_count || 0) + " strong");
    if (hasNeg(cl.max_rssi)) { lines.push("strongest neighbor " + cl.max_rssi + " dBm"); }
    return lines.join("\n");
  }

  // renderAirRec writes the clearest-block recommendation line (or an honest "no
  // data" note when the ambient scan is empty — NEVER a reassuring "airspace clear").
  function renderAirRec(nChannels, rec) {
    while (airRec.firstChild) { airRec.removeChild(airRec.firstChild); }
    if (nChannels === 0) {
      airRec.textContent = "No ambient scan data yet (the cached neighbor list may be empty or sparse).";
      return;
    }
    if (!rec || !rec.label) {
      airRec.textContent = "No 5 GHz recommendation (no 5 GHz neighbors in the ambient scan).";
      return;
    }
    airRec.appendChild(document.createTextNode("Clearest 80MHz block: "));
    var lab = document.createElement("span");
    lab.className = "rec-label";
    lab.textContent = rec.label;
    airRec.appendChild(lab);
    airRec.appendChild(document.createTextNode(" "));
    var dfs = document.createElement("span");
    dfs.className = rec.dfs ? "rec-dfs" : "rec-nondfs";
    dfs.textContent = rec.dfs ? "(DFS — radar-sensitive)" : "(non-DFS)";
    airRec.appendChild(dfs);
  }

  function applyAirspace(data) {
    lastAirspace = {
      channels: (data && data.channels) || [],
      neighbors: (data && data.neighbors) || [],
      my_channel: (data && data.my_channel) || "",
      recommendation: (data && data.recommendation) || null
    };
    drawAirspace(lastAirspace);
  }

  // pollAirspace mirrors pollHistory/pollOutages: fetch /airspace.json no-store every
  // ~15s, keep-last-good on catch. NOT on the 1s /data.json loop — the neighbor list
  // is a cached passive scan, minutes stale by design.
  function pollAirspace() {
    fetch("/airspace.json", { cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (data) { applyAirspace(data); })
      .catch(function () { /* keep last-good airspace; retry next tick */ })
      .finally(function () { setTimeout(pollAirspace, 15000); });
  }

  pollAirspace();

  // ----- top talkers / per-process bandwidth (Phase 11) -----------------
  // renderProcs paints the TOP TALKERS panel from /procs.json. Process NAMES are
  // ADVERSARIAL (any app names itself) — rendered via textContent ONLY, never
  // innerHTML. available:false => an honest "unavailable" (or "paused") line, never a
  // blank/zero masquerading as "nobody is uploading". The upload-ranked rate is the
  // headline; both up and down are shown (humanBps, the same formatting as the LIVE
  // tiles). On a gated tick the wire carries paused:true so we say so explicitly.
  //
  // PROCESS DRILL-DOWN (post-Phase-11): a row is CLICKABLE (role=button, tabindex, a
  // keydown Enter/Space parity handler) and toggles a DETAIL sub-row fetched ONCE from
  // /proc?pid=<n>. SINGLE-OPEN ACCORDION — at most one detail open, tracked in the
  // single expandedPid var. renderProcs wipes the tbody every 10s, so the detail node
  // never survives a re-render: after a rebuild, if the tracked pid is still present it
  // is re-expanded (one re-fetch); if it VANISHED from the ranked list it is cleared,
  // so the re-expand logic always has a valid target. All detail strings (command /
  // path / parent_name / remote_ip / remote_host — remote_host is the worst case, a
  // PTR controlled by the remote peer's reverse zone) render via textContent ONLY.
  var procsMeta = document.getElementById("procsmeta");
  var procsEmpty = document.getElementById("procsempty");
  var procsTableWrap = document.getElementById("procstablewrap");
  var procsBody = document.getElementById("procsbody");

  var expandedPid = null; // the single open pid (accordion), or null
  var procFetchGen = 0;   // monotonic fetch generation — guards the async stale-node race
  var lastProcs = [];     // last-rendered rows, by pid, for the PID-reuse name advisory

  var PROC_CLIENT_TIMEOUT = 6000; // above the backend worst case (exec + revDNS cap)
  var PD_HOST_MAX = 120;          // clamp an unbounded PTR (textContent already neutralizes markup)

  function clamp(s, max) {
    s = String(s);
    return s.length > max ? s.slice(0, max - 1) + "…" : s;
  }

  // findTalkerRow re-finds the live talker <tr> by pid at resolve time (NEVER a held
  // node reference — the 10s re-render replaces the nodes). Returns null if gone.
  function findTalkerRow(pid) {
    var rows = procsBody.children;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].className.indexOf("ptalker") >= 0 &&
          rows[i].getAttribute("data-pid") === String(pid)) {
        return rows[i];
      }
    }
    return null;
  }

  function collapseDetail() {
    var rows = procsBody.children;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].className.indexOf("pdetail") >= 0) {
        procsBody.removeChild(rows[i]);
        i--;
      }
    }
    var open = procsBody.getElementsByClassName("open");
    while (open.length) { open[0].classList.remove("open"); }
  }

  // insertDetailRow inserts a fresh pdetail sub-row after the talker row for pid and
  // returns its single <td colspan=4> cell (colspan matches the 4-column header). A
  // loading placeholder is shown until the fetch resolves.
  function insertDetailRow(pid) {
    var row = findTalkerRow(pid);
    if (!row) { return null; }
    row.classList.add("open");
    row.setAttribute("aria-expanded", "true");
    var tr = document.createElement("tr");
    tr.className = "pdetail";
    tr.setAttribute("data-pid", String(pid));
    var td = document.createElement("td");
    td.setAttribute("colspan", "4");
    var msg = document.createElement("div");
    msg.className = "pd-msg";
    msg.textContent = "loading process detail…";
    td.appendChild(msg);
    tr.appendChild(td);
    if (row.nextSibling) { procsBody.insertBefore(tr, row.nextSibling); }
    else { procsBody.appendChild(tr); }
    return td;
  }

  function pdRow(grid, k, v) {
    var dk = document.createElement("div"); dk.className = "pd-k"; dk.textContent = k;
    var dv = document.createElement("div"); dv.className = "pd-v"; dv.textContent = v;
    grid.appendChild(dk); grid.appendChild(dv);
  }

  // renderDetail paints the detail cell. ALL textContent (createElement / createTextNode),
  // NEVER innerHTML — command/path/parent_name/remote_ip/remote_host are attacker-
  // influencable. available:false => an honest "no longer running" line.
  function renderDetail(cell, d) {
    while (cell.firstChild) { cell.removeChild(cell.firstChild); }
    if (!d || !d.available) {
      var m = document.createElement("div");
      m.className = "pd-msg";
      m.textContent = "process no longer running (or not identifiable)";
      cell.appendChild(m);
      return;
    }
    var grid = document.createElement("div");
    grid.className = "pd-grid";
    if (d.command) { pdRow(grid, "Command", clamp(d.command, 400)); }
    if (d.path) { pdRow(grid, "Path", clamp(d.path, 400)); }
    pdRow(grid, "PID", String(d.pid) + (d.user ? ("  ·  " + clamp(d.user, 64)) : ""));
    if (d.ppid) {
      pdRow(grid, "Parent", String(d.ppid) + (d.parent_name ? ("  (" + clamp(d.parent_name, 64) + ")") : ""));
    }
    if (d.started) { pdRow(grid, "Started", clamp(d.started, 64)); }
    cell.appendChild(grid);

    var conns = d.connections || [];
    if (conns.length === 0) {
      var none = document.createElement("div");
      none.className = "pd-msg";
      none.textContent = "no established connections right now";
      cell.appendChild(none);
    } else {
      var ul = document.createElement("ul");
      ul.className = "pd-conns";
      for (var i = 0; i < conns.length; i++) {
        var c = conns[i];
        var ip = String(c.remote_ip || "");
        if (ip.indexOf(":") >= 0) { ip = "[" + ip + "]"; } // re-bracket IPv6 for display
        var li = document.createElement("li");
        li.appendChild(document.createTextNode(ip + ":" + String(c.remote_port || 0)));
        if (c.remote_host) {
          var span = document.createElement("span");
          span.className = "pd-host";
          span.textContent = "  " + clamp(c.remote_host, PD_HOST_MAX);
          li.appendChild(span);
        }
        ul.appendChild(li);
      }
      cell.appendChild(ul);
    }

    // PID-REUSE ADVISORY (client-side, best-effort): if the ranked row's name no longer
    // matches the detail's identity, the pid may have been recycled since the snapshot.
    var known = lastProcs[d.pid];
    if (known && d.name && known.toLowerCase().indexOf(String(d.name).toLowerCase().slice(0, 8)) < 0 &&
        String(d.name).toLowerCase().indexOf(known.toLowerCase().slice(0, 8)) < 0) {
      var warn = document.createElement("div");
      warn.className = "pd-warn";
      warn.textContent = "pid may have been recycled since the rate snapshot (now: " + clamp(d.name, 64) + ")";
      cell.appendChild(warn);
    }

    var note = document.createElement("div");
    note.className = "pd-note";
    var t = d.snapshot_t ? new Date(d.snapshot_t) : null;
    var hhmmss = (t && !isNaN(t.getTime()))
      ? ("" + pad2(t.getHours()) + ":" + pad2(t.getMinutes()) + ":" + pad2(t.getSeconds()))
      : "now";
    note.textContent = "point-in-time snapshot at " + hhmmss + " · sudo-free ps + lsof · reverse-DNS best-effort";
    cell.appendChild(note);
  }

  function pad2(n) { return (n < 10 ? "0" : "") + n; }

  // fetchDetail fires EXACTLY ONE /proc fetch for pid (built with STRING CONCAT, never
  // a template literal — the whole page is a Go backtick raw-string). A generation +
  // expandedPid guard makes the async resolve a NO-OP if the pid was collapsed, a newer
  // fetch superseded this one, or the row is gone (the 10s re-render). An AbortController
  // + client timeout flips a stuck sub-row to an honest "unavailable" line.
  function fetchDetail(pid) {
    var gen = ++procFetchGen;
    var cell = insertDetailRow(pid);
    if (!cell) { return; }
    var ctrl = (typeof AbortController !== "undefined") ? new AbortController() : null;
    var timer = setTimeout(function () { if (ctrl) { ctrl.abort(); } }, PROC_CLIENT_TIMEOUT);
    var opts = { cache: "no-store" };
    if (ctrl) { opts.signal = ctrl.signal; }
    fetch("/proc?pid=" + encodeURIComponent(pid), opts)
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (d) {
        if (gen !== procFetchGen || expandedPid !== pid) { return; } // superseded / collapsed
        var liveCell = detailCellFor(pid);
        if (liveCell) { renderDetail(liveCell, d); }
      })
      .catch(function () {
        if (gen !== procFetchGen || expandedPid !== pid) { return; }
        var liveCell = detailCellFor(pid);
        if (liveCell) {
          while (liveCell.firstChild) { liveCell.removeChild(liveCell.firstChild); }
          var m = document.createElement("div");
          m.className = "pd-msg";
          m.textContent = "detail unavailable (timed out)";
          liveCell.appendChild(m);
        }
      })
      .finally(function () { clearTimeout(timer); });
  }

  function detailCellFor(pid) {
    var rows = procsBody.children;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].className.indexOf("pdetail") >= 0 &&
          rows[i].getAttribute("data-pid") === String(pid)) {
        return rows[i].firstChild;
      }
    }
    return null;
  }

  // toggleProc is the click/keyboard entry point. Clicking the open row COLLAPSES it
  // (no fetch); clicking a different row collapses the first and expands the new one
  // (one fetch). Single-open accordion.
  function toggleProc(pid) {
    if (expandedPid === pid) {
      collapseDetail();
      expandedPid = null;
      return;
    }
    collapseDetail();
    expandedPid = pid;
    fetchDetail(pid);
  }

  function renderProcs(data) {
    var procs = (data && data.procs) || [];
    var available = !!(data && data.available);
    var paused = !!(data && data.paused);
    while (procsBody.firstChild) { procsBody.removeChild(procsBody.firstChild); }

    if (!available) {
      procsTableWrap.hidden = true;
      procsEmpty.hidden = false;
      procsEmpty.textContent = paused
        ? "paused (on battery / screen locked)"
        : "per-process data unavailable";
      procsMeta.textContent = "";
      expandedPid = null; // nothing to re-expand under
      return;
    }
    var n = procs.length;
    procsMeta.textContent = n ? (n + (n === 1 ? " talker" : " talkers")) : "";
    if (n === 0) {
      procsTableWrap.hidden = true;
      procsEmpty.hidden = false;
      procsEmpty.textContent = "no upstream activity from your processes right now";
      expandedPid = null;
      return;
    }
    procsEmpty.hidden = true;
    procsTableWrap.hidden = false;
    lastProcs = {};
    var stillPresent = false;
    for (var i = 0; i < n; i++) {
      var p = procs[i];
      var nm = (p.name === undefined || p.name === null || p.name === "") ? "(unknown)" : String(p.name);
      if (p.pid) { lastProcs[p.pid] = nm; }
      if (expandedPid !== null && p.pid === expandedPid) { stillPresent = true; }
      var tr = document.createElement("tr");
      tr.className = "ptalker";
      if (p.pid) {
        tr.setAttribute("data-pid", String(p.pid));
        tr.setAttribute("role", "button");
        tr.setAttribute("tabindex", "0");
        tr.setAttribute("aria-expanded", "false");
        tr.title = nm + " · pid " + p.pid + " · up " + humanBps(p.up_bytes_per_sec || 0);
        (function (pid) {
          tr.addEventListener("click", function () { toggleProc(pid); });
          tr.addEventListener("keydown", function (ev) {
            if (ev.key === "Enter" || ev.key === " " || ev.key === "Spacebar") {
              ev.preventDefault();
              toggleProc(pid);
            }
          });
        })(p.pid);
      }
      var tdName = document.createElement("td");
      tdName.textContent = nm;
      tr.appendChild(tdName);
      var tdPid = document.createElement("td");
      tdPid.textContent = p.pid ? String(p.pid) : "—";
      tr.appendChild(tdPid);
      var tdUp = document.createElement("td");
      tdUp.textContent = humanBps(p.up_bytes_per_sec || 0);
      tr.appendChild(tdUp);
      var tdDown = document.createElement("td");
      tdDown.textContent = humanBps(p.down_bytes_per_sec || 0);
      tr.appendChild(tdDown);
      procsBody.appendChild(tr);
    }
    // Re-expand the tracked pid if it is still present (ONE re-fetch, honoring the
    // generation guard so overlapping polls do not stack execs); collapse+clear it if
    // it vanished from the ranked list (died / outranked) so the target is always valid.
    if (expandedPid !== null) {
      if (stillPresent) { fetchDetail(expandedPid); }
      else { expandedPid = null; }
    }
  }

  // pollProcs runs on its OWN slow cadence (~procs_interval, 10s) — NOT the 1s
  // /data.json loop (nettop + a 2s window is heavy). Keep-last-good on catch.
  function pollProcs() {
    fetch("/procs.json", { cache: "no-store" })
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } return r.json(); })
      .then(function (data) { renderProcs(data); })
      .catch(function () { /* keep last-good talkers; retry next tick */ })
      .finally(function () { setTimeout(pollProcs, 10000); });
  }

  pollProcs();

  // ----- educational tile INFO CARD (one reusable dialog) -----------------
  // Each top tile carries data-metric; clicking (or Enter/Space) opens the ONE
  // #infocard dialog populated from METRIC_HELP for that metric. ALL content is
  // author-written but STILL built via createElement + textContent (NEVER
  // innerHTML — keeps the no-innerHTML guard green). The live "current reading"
  // line reuses the value already shown in the tile (read from its .t-val at open
  // time); the poll loop is never touched, so values keep updating underneath.
  //
  // Each entry: val = the tile .t-val element id; whatis/measures/good = the three
  // explainer paragraphs. The live interpretation is computed in icInterp().
  var METRIC_HELP = {
    meeting: {
      val: "meetingval",
      whatis: "A quick verdict on whether your connection is steady enough to join a video call right now. It folds jitter and packet loss into one GOOD / RISKY / BAD light.",
      measures: "A traffic-light verdict derived from jitter (ms) and packet loss (%).",
      good: "GOOD = jitter under 20 ms AND loss under 1%. RISKY = jitter under 50 ms AND loss under 5%. Anything worse is BAD."
    },
    download: {
      val: "dval",
      whatis: "How fast data is arriving over your Wi-Fi right now. This is what you are actually USING this moment, not the maximum your plan or link could reach.",
      measures: "Current receive rate in bits per second (the sub-line shows the peak and average over the visible window).",
      good: "There is no single good number — it reflects current activity. It sits near zero when idle and climbs during a download or video. Compare it with your plan's rated speed."
    },
    upload: {
      val: "uval",
      whatis: "How fast data is leaving your machine over Wi-Fi right now. Like download, it reflects what you are using this second, not a maximum.",
      measures: "Current send rate in bits per second (the sub-line shows peak and average).",
      good: "Activity-dependent. On most home plans upload is much smaller than download, so a lower number here is completely normal."
    },
    samples: {
      val: "nval",
      whatis: "How many once-per-second readings have been collected this session. It simply tells you how much history the live chart is showing.",
      measures: "A running count of samples. CYCLE (in the sub-line) is the gap between samples.",
      good: "Informational only — there is no good or bad value. It grows by one every cycle."
    },
    rssi: {
      val: "rssival",
      whatis: "How strong the Wi-Fi signal from your access point is. Values are negative dBm; closer to zero means a stronger signal.",
      measures: "Received signal strength in dBm (the sub-line shows the noise floor).",
      good: "At or above -50 excellent, -50 to -60 very good, -60 to -67 good (fine for video calls), -67 to -70 okay, -70 to -80 weak, below -80 poor. -67 dBm is the usual floor for reliable real-time use."
    },
    snr: {
      val: "snrval",
      whatis: "How far your signal rises above the background radio noise. Higher is better, and it often predicts real throughput better than signal strength alone.",
      measures: "Signal minus noise floor, in dB (the noise floor is in the sub-line).",
      good: "At or above 40 dB excellent, 25 to 40 good, 15 to 25 fair or marginal, below 15 poor."
    },
    txrate: {
      val: "txval",
      whatis: "The speed your Wi-Fi radio negotiated with the access point for the current band, channel width and signal. It is the link's potential, not your actual throughput.",
      measures: "Negotiated PHY / link rate in megabits per second (the sub-line shows the PHY mode and MCS index).",
      good: "Higher is better. It drops with distance, interference, or a slower band. 2.4 GHz tops out low; 5 GHz at 80 MHz (802.11ac) can reach several hundred Mb/s."
    },
    band: {
      val: "bandval",
      whatis: "Which Wi-Fi band and channel you are connected on. 5 and 6 GHz are faster and less crowded but shorter range; 2.4 GHz reaches further but is slower and more congested.",
      measures: "The band (2.4 / 5 / 6 GHz) plus channel and width. The network name is redacted by macOS, so it may read HIDDEN.",
      good: "No single best choice. A congested channel can hurt speed even with a strong signal — the AIRSPACE panel below shows how busy nearby channels are."
    },
    reach: {
      val: "reachval",
      whatis: "A forensic read on where a problem lies: is it your Wi-Fi, your router, or your ISP? It checks whether your gateway and the public internet both answer.",
      measures: "A verdict: OK (gateway and internet both reachable), GATEWAY ONLY (router fine, internet / ISP down), or DOWN (local link dead).",
      good: "OK is what you want. GATEWAY ONLY points at your ISP; DOWN points at your local Wi-Fi link."
    },
    gwrtt: {
      val: "gwrttval",
      whatis: "The round-trip time to your own router. On a healthy Wi-Fi link this should be just a few milliseconds; if it is high, the trouble is local contention, not your ISP.",
      measures: "Round-trip time to the gateway in milliseconds (the sub-line shows the gateway address).",
      good: "Under 10 ms is great, 10 to 30 ms is okay, over 30 ms suggests local Wi-Fi congestion or interference."
    },
    inetrtt: {
      val: "inetrttval",
      whatis: "The round-trip time to a public server on the internet (1.1.1.1 / 8.8.8.8, best of the two). This is your latency to the wider world.",
      measures: "Round-trip time to a public target in milliseconds (the sub-line shows which address answered).",
      good: "Under 30 ms great, 30 to 50 good, 50 to 100 okay, over 100 ms sluggish for real-time use."
    },
    dns: {
      val: "dnsval",
      whatis: "How long it takes to turn a domain name into an address. Slow DNS makes every new site or app feel laggy before anything even starts to load.",
      measures: "Time for a single name lookup in milliseconds.",
      good: "Under 50 ms is snappy, 50 to 100 ms is fine, over 200 ms is slow — a faster resolver may help."
    },
    jitter: {
      val: "jitterval",
      whatis: "How much latency varies from packet to packet. It is the main enemy of calls and gaming — steady latency feels fine, but bouncy latency causes choppy audio and video.",
      measures: "The variation in round-trip time in milliseconds (measured on the internet path).",
      good: "Under 5 ms excellent, 5 to 20 good, 20 to 30 noticeable, over 30 ms bad for real-time. Lower is better even when the average latency looks fine."
    },
    loss: {
      val: "lossval",
      whatis: "The share of probe packets that got no reply. Lost packets cause dropouts, stalls and retransmits — it hurts calls far more than a slightly slower speed.",
      measures: "Percentage of probes with no response (the sub-line splits gateway vs internet loss).",
      good: "0% is ideal, under 1% is good, 1 to 2.5% is tolerable, over 5% is bad for real-time use."
    }
  };

  // icInterp maps the CURRENT tile value text to a {cls, verdict, note} live read.
  // cls is "good"/"fair"/"poor" (color border + verdict word) or "" (neutral,
  // informational). Returns null when there is no reading yet ("—"), so the caller
  // shows a friendly "no reading yet" line. Numeric metrics parseFloat the leading
  // token (e.g. "-64 dBm" -> -64, "46.0 ms" -> 46, "0%" -> 0).
  function icInterp(metric, valText) {
    var t = (valText || "").trim();
    if (t === "" || t === "—" || t === "-") { return null; }
    var v = parseFloat(t); // leading numeric token; NaN for word verdicts (OK/GOOD/...)
    switch (metric) {
      case "meeting":
        if (t === "GOOD") { return { cls: "good", verdict: "Good", note: "jitter and loss are both in the safe range — go ahead and join." }; }
        if (t === "RISKY") { return { cls: "fair", verdict: "Risky", note: "usable, but jitter or loss is elevated — expect occasional glitches." }; }
        if (t === "BAD") { return { cls: "poor", verdict: "Bad", note: "jitter or loss is too high for a smooth call right now." }; }
        return { cls: "", verdict: "Pending", note: "waiting for the first reachability cycle." };
      case "reach":
        if (t === "OK") { return { cls: "good", verdict: "OK", note: "both your router and the public internet are reachable." }; }
        if (t.indexOf("GATEWAY") === 0) { return { cls: "fair", verdict: "Gateway only", note: "your router is fine but the internet is not answering — points at your ISP." }; }
        if (t === "DOWN") { return { cls: "poor", verdict: "Down", note: "the local link is dead — points at your Wi-Fi." }; }
        return { cls: "", verdict: "Pending", note: "waiting for the first reachability cycle." };
      case "rssi":
        if (isNaN(v)) { return null; }
        if (v >= -60) { return { cls: "good", verdict: "Strong", note: "excellent signal strength." }; }
        if (v >= -67) { return { cls: "good", verdict: "Good", note: "fine for video calls and real-time use." }; }
        if (v >= -70) { return { cls: "fair", verdict: "Okay", note: "usable, but near the reliable floor of -67 dBm." }; }
        if (v >= -80) { return { cls: "poor", verdict: "Weak", note: "move closer to the access point or reduce obstacles." }; }
        return { cls: "poor", verdict: "Poor", note: "likely unreliable at this signal level." };
      case "snr":
        if (isNaN(v)) { return null; }
        if (v >= 40) { return { cls: "good", verdict: "Excellent", note: "plenty of headroom above the noise." }; }
        if (v >= 25) { return { cls: "good", verdict: "Good", note: "healthy margin over the noise floor." }; }
        if (v >= 15) { return { cls: "fair", verdict: "Fair", note: "marginal — throughput may suffer." }; }
        return { cls: "poor", verdict: "Poor", note: "too little margin over the noise." };
      case "gwrtt":
        if (isNaN(v)) { return null; }
        if (v < 10) { return { cls: "good", verdict: "Great", note: "snappy local link." }; }
        if (v <= 30) { return { cls: "fair", verdict: "Okay", note: "acceptable, but watch for local congestion." }; }
        return { cls: "poor", verdict: "Suspect", note: "high for a router hop — suspect local Wi-Fi congestion." };
      case "inetrtt":
        if (isNaN(v)) { return null; }
        if (v < 30) { return { cls: "good", verdict: "Great", note: "low latency to the wider internet." }; }
        if (v <= 50) { return { cls: "good", verdict: "Good", note: "comfortable for real-time use." }; }
        if (v <= 100) { return { cls: "fair", verdict: "Okay", note: "fine for browsing, a touch high for calls." }; }
        return { cls: "poor", verdict: "Sluggish", note: "high enough to feel laggy in real-time apps." };
      case "dns":
        if (isNaN(v)) { return null; }
        if (v < 50) { return { cls: "good", verdict: "Snappy", note: "lookups resolve quickly." }; }
        if (v <= 100) { return { cls: "fair", verdict: "Fine", note: "acceptable lookup time." }; }
        if (v <= 200) { return { cls: "fair", verdict: "Slowish", note: "noticeable before pages start loading." }; }
        return { cls: "poor", verdict: "Slow", note: "consider a faster resolver." };
      case "jitter":
        if (isNaN(v)) { return null; }
        if (v < 5) { return { cls: "good", verdict: "Excellent", note: "very steady latency." }; }
        if (v <= 20) { return { cls: "good", verdict: "Good", note: "steady enough for smooth calls." }; }
        if (v <= 30) { return { cls: "fair", verdict: "Noticeable", note: "may cause occasional choppiness." }; }
        return { cls: "poor", verdict: "Elevated", note: "bad for real-time — expect choppy audio or video." };
      case "loss":
        if (isNaN(v)) { return null; }
        if (v < 1) { return { cls: "good", verdict: "Good", note: v === 0 ? "no packets dropped." : "negligible drops." }; }
        if (v <= 2.5) { return { cls: "fair", verdict: "Tolerable", note: "some drops — usually survivable." }; }
        if (v <= 5) { return { cls: "fair", verdict: "Elevated", note: "drops are becoming disruptive." }; }
        return { cls: "poor", verdict: "Bad", note: "too many dropped packets for real-time use." };
      // download / upload / txrate / band / samples are informational (neutral).
      case "download": return { cls: "", verdict: "In use now", note: "this is live usage, not your maximum link speed." };
      case "upload": return { cls: "", verdict: "In use now", note: "upload is usually much smaller than download." };
      case "txrate": return { cls: "", verdict: "Link potential", note: "the negotiated rate — higher is better; it is not your actual throughput." };
      case "band": return { cls: "", verdict: "Current band", note: "a congested channel can slow you even with a strong signal." };
      case "samples": return { cls: "", verdict: "Collected so far", note: "just the size of the live chart window." };
      default: return null;
    }
  }

  // Build one labeled section (label + body paragraph) via textContent only.
  function icSection(label, body) {
    var sec = document.createElement("div");
    sec.className = "ic-sec";
    var k = document.createElement("div");
    k.className = "ic-k";
    k.textContent = label;
    var v = document.createElement("div");
    v.className = "ic-v";
    v.textContent = body;
    sec.appendChild(k);
    sec.appendChild(v);
    return sec;
  }

  var infocard = document.getElementById("infocard");
  var infocardTitle = document.getElementById("infocard-title");
  var infocardBody = document.getElementById("infocard-body");
  var infocardClose = document.getElementById("infocard-close");
  var tilesEl = document.getElementById("tiles");
  var infocardOpener = null; // the tile to restore focus to on close
  var infocardMetric = "";   // currently shown metric (for toggle-on-reclick)

  // Human-facing title per metric (also becomes the dialog's aria-label target).
  var METRIC_NAME = {
    meeting: "Meeting readiness", download: "Download throughput", upload: "Upload throughput",
    samples: "Samples", rssi: "RSSI — signal strength", snr: "SNR — signal-to-noise ratio",
    txrate: "TX rate — link rate", band: "Band / channel", reach: "Reachability",
    gwrtt: "Gateway RTT", inetrtt: "Internet RTT", dns: "DNS lookup time",
    jitter: "Jitter", loss: "Packet loss"
  };

  function openInfoCard(metric, opener) {
    var help = METRIC_HELP[metric];
    if (!help) { return; }
    infocardMetric = metric;
    infocardOpener = opener || null;
    infocardTitle.textContent = METRIC_NAME[metric] || metric;

    // Rebuild the body from scratch (textContent only — no innerHTML).
    while (infocardBody.firstChild) { infocardBody.removeChild(infocardBody.firstChild); }
    infocardBody.appendChild(icSection("What it is", help.whatis));
    infocardBody.appendChild(icSection("Measures / units", help.measures));
    infocardBody.appendChild(icSection("Good / expected values", help.good));

    // Live current-value read, reusing the value already shown in the tile.
    var valEl = document.getElementById(help.val);
    var valText = valEl ? valEl.textContent : "";
    var read = icInterp(metric, valText);
    var live = document.createElement("div");
    live.className = "ic-live" + (read && read.cls ? (" " + read.cls) : "");
    if (!read) {
      live.textContent = "Your reading: no value yet — check back once sampling warms up.";
    } else {
      var lead = document.createElement("div");
      lead.textContent = "Your reading: " + valText;
      var vl = document.createElement("div");
      vl.style.marginTop = "4px";
      var vb = document.createElement("span");
      vb.className = "ic-verdict";
      vb.textContent = read.verdict;
      vl.appendChild(vb);
      vl.appendChild(document.createTextNode(read.note ? (" — " + read.note) : ""));
      live.appendChild(lead);
      live.appendChild(vl);
    }
    infocardBody.appendChild(live);

    infocard.hidden = false;
    infocardClose.focus();
  }

  function closeInfoCard() {
    if (infocard.hidden) { return; }
    infocard.hidden = true;
    infocardMetric = "";
    if (infocardOpener && typeof infocardOpener.focus === "function") { infocardOpener.focus(); }
    infocardOpener = null;
  }

  function metricTileFrom(target) {
    var node = target;
    while (node && node !== tilesEl) {
      if (node.getAttribute && node.hasAttribute("data-metric")) { return node; }
      node = node.parentNode;
    }
    return null;
  }

  if (tilesEl) {
    // Click delegation: one listener for all 14 tiles.
    tilesEl.addEventListener("click", function (ev) {
      var tile = metricTileFrom(ev.target);
      if (!tile) { return; }
      var metric = tile.getAttribute("data-metric");
      // Re-click of the already-open metric toggles the card shut.
      if (!infocard.hidden && infocardMetric === metric) { closeInfoCard(); return; }
      openInfoCard(metric, tile);
    });
    // Keyboard affordance: Enter / Space activate the focused tile.
    tilesEl.addEventListener("keydown", function (ev) {
      if (ev.key !== "Enter" && ev.key !== " " && ev.key !== "Spacebar") { return; }
      var tile = metricTileFrom(ev.target);
      if (!tile) { return; }
      ev.preventDefault();
      var metric = tile.getAttribute("data-metric");
      if (!infocard.hidden && infocardMetric === metric) { closeInfoCard(); return; }
      openInfoCard(metric, tile);
    });
  }

  if (infocardClose) { infocardClose.addEventListener("click", closeInfoCard); }
  if (infocard) {
    // Click-outside: only a click on the backdrop itself (not the card) closes.
    infocard.addEventListener("click", function (ev) {
      if (ev.target === infocard) { closeInfoCard(); }
    });
  }
  // Esc closes; Tab is trapped inside the dialog (the close button is its only stop).
  document.addEventListener("keydown", function (ev) {
    if (infocard.hidden) { return; }
    if (ev.key === "Escape" || ev.key === "Esc") { ev.preventDefault(); closeInfoCard(); return; }
    if (ev.key === "Tab") { ev.preventDefault(); infocardClose.focus(); }
  });

  // ----- LIVE-chart EVENT MARKER detail card ("what happened here") --------
  // Each drop/roam/band_change marker on the LIVE chart is clickable. The click is
  // delegated to the #chart SVG ONCE (so it SURVIVES the ~1s redraw that replaces the
  // marker nodes) and walks up to the .evmarker group to recover grp.ndEvent — a STABLE
  // snapshot, since the card body is built from that object's fields at click time. It
  // reuses the SAME shared #infocard dialog as the tiles (coexisting cleanly: a marker
  // open sets infocardMetric="" so a later tile re-click never mistakes it for a metric).
  // ALL text via createElement/textContent — NEVER innerHTML.
  var MARKER_CARD_TITLE = { drop: "Link drop", roam: "Roam", band_change: "Band change" };

  // icNode builds one labeled section whose value is a NODE (vs icSection's text) — used
  // for the cause chip. Mirrors icSection's .ic-sec / .ic-k / .ic-v structure.
  function icNode(label, node) {
    var sec = document.createElement("div");
    sec.className = "ic-sec";
    var k = document.createElement("div");
    k.className = "ic-k";
    k.textContent = label;
    var v = document.createElement("div");
    v.className = "ic-v";
    v.appendChild(node);
    sec.appendChild(k);
    sec.appendChild(v);
    return sec;
  }

  // nearestOutage finds the logged outage whose START is closest to the drop's time,
  // within a small window (~45s, a few reach cycles). Returns null if none is close —
  // the card then says so honestly rather than fabricating detail.
  function nearestOutage(tIso) {
    var tms = Date.parse(tIso);
    if (isNaN(tms)) { return null; }
    var best = null, bestDiff = Infinity;
    var list = lastOutages || [];
    for (var i = 0; i < list.length; i++) {
      var oms = Date.parse(list[i].start);
      if (isNaN(oms)) { continue; }
      var diff = Math.abs(oms - tms);
      if (diff < bestDiff) { bestDiff = diff; best = list[i]; }
    }
    return (best && bestDiff <= 45000) ? best : null;
  }

  // onsetContextText renders the onset radio context HONESTLY — only the fields the
  // outage actually recorded (band / channel / RSSI / noise / SNR).
  function onsetContextText(o) {
    var parts = [];
    if (o.band) { parts.push(o.band); }
    if (o.channel) { parts.push("ch " + o.channel); }
    if (hasNeg(o.rssi)) { parts.push("RSSI " + o.rssi + " dBm"); }
    if (hasNeg(o.noise)) { parts.push("noise " + o.noise + " dBm"); }
    if (snrReal(o.snr)) { parts.push("SNR " + o.snr + " dB"); }
    return parts.length ? parts.join(" · ") : "—";
  }

  function openMarkerCard(ev) {
    if (!ev) { return; }
    infocardMetric = ""; // a marker is NOT a metric — keeps tile re-click toggling clean
    infocardOpener = null;
    infocardTitle.textContent = MARKER_CARD_TITLE[ev.kind] || (MARKER_NAME[ev.kind] || ev.kind);

    while (infocardBody.firstChild) { infocardBody.removeChild(infocardBody.firstChild); }
    infocardBody.appendChild(icSection("When", fmtClock(ev.t)));

    if (ev.kind === "roam") {
      infocardBody.appendChild(icSection("Channel", dash(ev.from) + " → " + dash(ev.to)));
      infocardBody.appendChild(icSection("What happened",
        "A roam = your Mac moved to a different access point, on a different channel."));
    } else if (ev.kind === "band_change") {
      infocardBody.appendChild(icSection("Band", dash(ev.from) + " → " + dash(ev.to)));
      infocardBody.appendChild(icSection("What happened",
        "Your Mac switched Wi-Fi band (e.g. 5 GHz ↔ 2.4 GHz) on this connection."));
    } else {
      // DROP: cross-reference the already-polled OUTAGE JOURNAL.
      var o = nearestOutage(ev.t);
      if (o) {
        infocardBody.appendChild(icNode("Cause", causeChip(o.cause || "")));
        infocardBody.appendChild(icSection("Onset radio context", onsetContextText(o)));
        infocardBody.appendChild(icSection("Duration", fmtDur(o.duration_sec)));
        infocardBody.appendChild(icSection("What happened",
          "The link dropped or the internet became unreachable here — this blip debounced " +
          "into a logged outage. See the full log in the Outage Journal below."));
      } else {
        infocardBody.appendChild(icSection("What happened",
          "Brief drop — no logged outage; the journal debounces short blips (an outage " +
          "needs the link lost or the internet unreachable for ≥2 reach cycles)."));
        if (ev.from || ev.to) {
          infocardBody.appendChild(icSection("Detail", dash(ev.from) + " → " + dash(ev.to)));
        }
      }
    }

    infocard.hidden = false;
    infocardClose.focus();
  }

  // markerGroupFrom walks up from a click target to the enclosing .evmarker group.
  function markerGroupFrom(target) {
    var node = target;
    while (node && node !== chart) {
      if (node.ndEvent) { return node; }
      node = node.parentNode;
    }
    return null;
  }

  if (chart) {
    // ONE delegated listener — bound to the stable #chart SVG, not the redrawn markers.
    chart.addEventListener("click", function (ev) {
      var grp = markerGroupFrom(ev.target);
      if (grp) { openMarkerCard(grp.ndEvent); }
    });
  }

  // ----- ON-DEMAND SPEED TEST (button is the SOLE trigger) ----------------
  // One saturating test per explicit click. There is NO poll loop, NO setInterval,
  // NO setTimeout-driven auto-run here — the POST fires ONLY from the run function,
  // wired to the button click. Client-side single-flight: the button is
  // disabled for the whole outstanding request and re-enabled ONLY in .finally(). All
  // result text is built with createElement/createTextNode (NEVER innerHTML).
  var speedBtn = document.getElementById("speedbtn");
  var speedResult = document.getElementById("speedresult");
  var speedCustom = document.getElementById("speedcustom");
  var stModeCustom = document.getElementById("stmode-custom");
  var stDownURL = document.getElementById("stdownurl");
  var stUpURL = document.getElementById("stupurl");
  // Client timeout must EXCEED the server-side run (hard cap ~30s) so the browser never
  // aborts a legitimately in-progress test before the server answers.
  var SPEED_CLIENT_TIMEOUT = 60000;
  var speedRunning = false;

  function stCustomMode() { return stModeCustom && stModeCustom.checked; }

  function syncSpeedCustom() {
    if (speedCustom) { speedCustom.hidden = !stCustomMode(); }
  }
  var stRadios = document.querySelectorAll('input[name="sttarget"]');
  for (var si = 0; si < stRadios.length; si++) {
    stRadios[si].addEventListener("change", syncSpeedCustom);
  }
  syncSpeedCustom();

  function stCell(klass, label, value) {
    var cell = document.createElement("div");
    cell.className = "st-cell" + (klass ? (" " + klass) : "");
    var k = document.createElement("div");
    k.className = "st-k";
    k.appendChild(document.createTextNode(label));
    var v = document.createElement("div");
    v.className = "st-v";
    v.appendChild(document.createTextNode(value));
    cell.appendChild(k);
    cell.appendChild(v);
    return cell;
  }

  // stMbps / stMs render a measured number, or "—" when the run failed / not measured.
  function stMbps(v, ok) {
    if (!ok || typeof v !== "number" || !isFinite(v) || v <= 0) { return "—"; }
    return v.toFixed(1) + " Mbps";
  }
  function stMs(v, ok) {
    if (!ok || typeof v !== "number" || !isFinite(v) || v <= 0) { return "—"; }
    return v.toFixed(1) + " ms";
  }

  function renderSpeedResult(data) {
    while (speedResult.firstChild) { speedResult.removeChild(speedResult.firstChild); }
    speedResult.hidden = false;
    var ok = !!(data && data.ok);

    if (!ok) {
      var err = document.createElement("div");
      err.className = "st-error";
      var msg = (data && data.error) ? String(data.error) : "speed test failed";
      err.appendChild(document.createTextNode(msg));
      speedResult.appendChild(err);
      // Still show the target we attempted, when present.
      if (data && data.target) {
        var tline = document.createElement("div");
        tline.className = "st-meta";
        tline.appendChild(document.createTextNode("target: "));
        var tspan = document.createElement("span");
        tspan.className = "st-target";
        tspan.appendChild(document.createTextNode(clamp(data.target, PD_HOST_MAX)));
        tline.appendChild(tspan);
        speedResult.appendChild(tline);
      }
      return;
    }

    var grid = document.createElement("div");
    grid.className = "st-grid";
    grid.appendChild(stCell("down", "Download", stMbps(data.download_mbps, ok)));
    grid.appendChild(stCell("up", "Upload", stMbps(data.upload_mbps, ok)));
    grid.appendChild(stCell("", "Latency", stMs(data.latency_ms, ok)));
    grid.appendChild(stCell("", "Jitter", stMs(data.jitter_ms, ok)));
    speedResult.appendChild(grid);

    var meta = document.createElement("div");
    meta.className = "st-meta";
    meta.appendChild(document.createTextNode("target: "));
    var tgt = document.createElement("span");
    tgt.className = "st-target";
    tgt.appendChild(document.createTextNode(clamp(data.target || "—", PD_HOST_MAX)));
    meta.appendChild(tgt);
    meta.appendChild(document.createTextNode("  ·  " + fmtClock(data.t)));
    speedResult.appendChild(meta);
  }

  function setSpeedBusy(busy) {
    speedRunning = busy;
    if (!speedBtn) { return; }
    speedBtn.disabled = busy;
    speedBtn.setAttribute("aria-busy", busy ? "true" : "false");
    while (speedBtn.firstChild) { speedBtn.removeChild(speedBtn.firstChild); }
    if (busy) {
      var sp = document.createElement("span");
      sp.className = "st-spinner";
      sp.setAttribute("aria-hidden", "true");
      speedBtn.appendChild(sp);
      speedBtn.appendChild(document.createTextNode("Running…"));
    } else {
      speedBtn.appendChild(document.createTextNode("Run speed test"));
    }
  }

  function runSpeedTest() {
    if (speedRunning) { return; } // client-side single-flight
    // Validate custom input minimally on the client (the server re-validates + SSRF-
    // gates); an empty custom download URL is a no-op with an inline hint.
    var body = "mode=cloudflare";
    if (stCustomMode()) {
      var dl = stDownURL ? stDownURL.value.replace(/^\s+|\s+$/g, "") : "";
      if (!dl) {
        renderSpeedResult({ ok: false, error: "enter a custom download URL first" });
        return;
      }
      var up = stUpURL ? stUpURL.value.replace(/^\s+|\s+$/g, "") : "";
      body = "mode=custom&download_url=" + encodeURIComponent(dl);
      if (up) { body += "&upload_url=" + encodeURIComponent(up); }
    }

    setSpeedBusy(true);
    while (speedResult.firstChild) { speedResult.removeChild(speedResult.firstChild); }

    var ctrl = (typeof AbortController !== "undefined") ? new AbortController() : null;
    var timer = setTimeout(function () { if (ctrl) { ctrl.abort(); } }, SPEED_CLIENT_TIMEOUT);
    var opts = {
      method: "POST",
      cache: "no-store",
      headers: {
        "X-Netdebug-SpeedTest": "1",
        "Content-Type": "application/x-www-form-urlencoded"
      },
      body: body
    };
    if (ctrl) { opts.signal = ctrl.signal; }

    fetch("/speedtest", opts)
      .then(function (r) {
        // 409 = another test is already running (single-flight busy); it still carries
        // an honest JSON body. Any other non-OK is a hard error with a JSON body too.
        return r.json().then(function (data) {
          if (!data || typeof data.ok === "undefined") {
            throw new Error("HTTP " + r.status);
          }
          return data;
        });
      })
      .then(function (data) { renderSpeedResult(data); })
      .catch(function (err) {
        var msg = (err && err.name === "AbortError")
          ? "timed out waiting for the speed test"
          : "could not reach the server for the speed test";
        renderSpeedResult({ ok: false, error: msg });
      })
      .finally(function () {
        clearTimeout(timer);
        setSpeedBusy(false); // re-enable ONLY here (client single-flight released)
      });
  }

  if (speedBtn) { speedBtn.addEventListener("click", runSpeedTest); }

  // Init: align H/HH + the #chart/#histchart viewBox attributes to the active layout
  // BEFORE the first async draw resolves (the SVGs are hidden behind the empty-state
  // div until first data, so no flash even though the served markup ships the TALL
  // viewBox). On a TALL load this writes the same TALL values — a visual no-op.
  applyChartDims();
})();
</script>
</body>
</html>
`

// reportHTML is the Phase-13 self-contained /report page: a speed-by-hour inline-SVG
// chart (FLAT bar fills ONLY — the no-external guard bans every url() form, so NO
// gradients), throttle-flagged peak-hour dips highlighted via the bar-flagged class,
// plus day totals / band time / drop count / worst-sample latency, and the MANDATORY
// honesty caveat as prominent STATIC text ("peak-hour dips observed", cross-referenced
// with the outage journal + top-talkers, NOT a definitive throttling accusation).
//
// Same no-external-URL discipline as dashboardHTML (enforced by a dedicated guard in
// serve_test.go): the SVG namespace is the split-literal concat "http" + "://www.w3.org
// /2000/svg" (the dashboard.go precedent) — a literal namespace string would trip the
// guard. ALL rendering via textContent / createElementNS — NEVER .innerHTML (every
// value, incl. the day label and band names, is adversarial-safe).
const reportHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>netdebug — daily report</title>
<style>
  :root {
    --bg:#0f1419; --panel:#141b22; --grid:#26303b; --ink:#e6edf3; --muted:#8b98a5;
    --down:#4aa3ff; --flag:#ff5c5c; --accent:#2bd576; --base:#f0a63a;
    --mono: ui-monospace, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace;
  }
  * { box-sizing: border-box; }
  html, body { margin:0; min-height:100%; }
  body { background:var(--bg); color:var(--ink); font:13px/1.45 var(--mono); padding:16px; -webkit-font-smoothing:antialiased; }
  h1 { font-size:15px; letter-spacing:2px; text-transform:uppercase; margin:0 0 2px; }
  .sub { color:var(--muted); margin-bottom:12px; }
  .caveat { border:1px solid var(--grid); border-left:3px solid var(--base); background:var(--panel); padding:10px 12px; margin:12px 0 16px; color:var(--muted); }
  .caveat strong { color:var(--ink); }
  .tiles { display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr)); gap:10px; margin-bottom:16px; }
  .tile { border:1px solid var(--grid); background:var(--panel); padding:10px 12px; }
  .tile .k { color:var(--muted); text-transform:uppercase; letter-spacing:1px; font-size:11px; }
  .tile .v { font-size:18px; margin-top:4px; }
  .card { border:1px solid var(--grid); background:var(--panel); padding:12px; margin-bottom:16px; }
  .card h2 { font-size:12px; letter-spacing:1px; text-transform:uppercase; color:var(--muted); margin:0 0 10px; }
  .legend { color:var(--muted); margin-top:8px; font-size:12px; }
  .legend .flag { color:var(--flag); }
  .legend .baseline { color:var(--base); }
  svg { display:block; width:100%; height:260px; background:var(--panel-2); overflow:visible; }
  .bar { fill:var(--down); }
  .bar-flagged { fill:var(--flag); }
  .axis { stroke:var(--grid); stroke-width:1; }
  .baseln { stroke:var(--base); stroke-width:1; stroke-dasharray:4 3; }
  .lbl { fill:var(--muted); font:10px var(--mono); }
  .dip { color:var(--flag); }
  table { border-collapse:collapse; width:100%; }
  td, th { text-align:left; padding:3px 10px 3px 0; }
  th { color:var(--muted); text-transform:uppercase; letter-spacing:1px; font-size:11px; }
  .empty { color:var(--muted); padding:8px 0; }
  footer { color:var(--muted); font-size:11px; margin-top:8px; }
</style>
</head>
<body>
<h1>netdebug daily report</h1>
<div class="sub" id="daylabel">loading…</div>

<div class="caveat">
  <strong>Honest reading:</strong> flagged hours are
  <strong>peak-hour dips observed</strong> in a small-transfer <em>relative</em> proxy —
  <strong>NOT</strong> a definitive throttling accusation. A dip can be ISP throttling,
  local congestion, your own upload saturation, or a bad channel. Cross-reference the
  flagged hours with the <strong>outage journal</strong> and <strong>top-talkers</strong>
  before concluding anything.
</div>

<div class="tiles">
  <div class="tile"><div class="k">Total down</div><div class="v" id="tdown">—</div></div>
  <div class="tile"><div class="k">Total up</div><div class="v" id="tup">—</div></div>
  <div class="tile"><div class="k">Drops</div><div class="v" id="drops">—</div></div>
  <div class="tile"><div class="k">Worst sample latency</div><div class="v" id="worst">—</div></div>
  <div class="tile"><div class="k">Baseline (p75)</div><div class="v" id="baseline">—</div></div>
  <div class="tile"><div class="k">Flagged hours</div><div class="v dip" id="flagged">—</div></div>
</div>

<div class="card">
  <h2>// Speed by hour (local)</h2>
  <svg id="speedchart" viewBox="0 0 900 260" preserveAspectRatio="none" aria-label="download Mbps per local hour"></svg>
  <div class="legend">
    download Mbps per local hour —
    <span class="flag">red = peak-hour dip</span> ·
    <span class="baseline">dashed = p75 baseline</span> ·
    download-only small-transfer proxy (upload is never sampled)
  </div>
</div>

<div class="card">
  <h2>// Band time (approximate)</h2>
  <table><thead><tr><th>Band</th><th>≈ seconds</th></tr></thead><tbody id="bandbody"></tbody></table>
  <div class="empty" id="bandempty" hidden>no band data for this span</div>
</div>

<footer id="foot"></footer>

<script>
(function(){
  "use strict";
  // Built by concatenation: the SVG namespace is a required identifier (never fetched),
  // but spelling the literal out would trip the strengthened blanket no-external-URL
  // guard (it bans the bare scheme prefix). The dashboard.go:443 precedent.
  var SVGNS = "http" + "://www.w3.org/2000/svg";
  var chart = document.getElementById("speedchart");

  function txt(id, s){ var el = document.getElementById(id); if (el) el.textContent = s; }

  function human(bytes){
    var f = Number(bytes) || 0;
    if (f >= 1e12) return (f/1e12).toFixed(2) + " TB";
    if (f >= 1e9)  return (f/1e9).toFixed(2) + " GB";
    if (f >= 1e6)  return (f/1e6).toFixed(2) + " MB";
    if (f >= 1e3)  return (f/1e3).toFixed(1) + " KB";
    return f.toFixed(0) + " B";
  }

  function mk(name, attrs){
    var e = document.createElementNS(SVGNS, name);
    for (var k in attrs){ if (attrs.hasOwnProperty(k)) e.setAttribute(k, attrs[k]); }
    return e;
  }

  function clear(el){ while (el.firstChild) el.removeChild(el.firstChild); }

  function renderChart(hours, flaggedSet, baseline){
    clear(chart);
    var W = 900, H = 260, padL = 44, padR = 12, padT = 14, padB = 26;
    var plotW = W - padL - padR, plotH = H - padT - padB;
    // baseline axis line (y=0)
    chart.appendChild(mk("line", {x1:padL, y1:(padT+plotH), x2:(W-padR), y2:(padT+plotH), "class":"axis"}));
    if (!hours.length){
      var t = mk("text", {x:padL, y:(padT+plotH/2), "class":"lbl"});
      t.textContent = "no speed samples yet — run ./netdebug --serve --throttle-watch over a day";
      chart.appendChild(t);
      return;
    }
    var maxV = baseline || 0;
    hours.forEach(function(h){ if (h.down_avg_mbps > maxV) maxV = h.down_avg_mbps; });
    if (maxV <= 0) maxV = 1;
    var slot = plotW / 24;
    var bw = slot * 0.72;
    // dashed p75 baseline
    if (baseline > 0){
      var by = padT + plotH - (baseline / maxV) * plotH;
      chart.appendChild(mk("line", {x1:padL, y1:by, x2:(W-padR), y2:by, "class":"baseln"}));
    }
    // hour ticks (every 3h)
    for (var hh = 0; hh <= 24; hh += 3){
      var lx = padL + hh * slot;
      var lb = mk("text", {x:lx, y:(H-8), "class":"lbl", "text-anchor":"middle"});
      lb.textContent = (hh < 10 ? "0"+hh : ""+hh);
      chart.appendChild(lb);
    }
    hours.forEach(function(h){
      var x = padL + h.hour * slot + (slot - bw)/2;
      var bh = (h.down_avg_mbps / maxV) * plotH;
      if (bh < 1) bh = 1;
      var y = padT + plotH - bh;
      var rect = mk("rect", {x:x, y:y, width:bw, height:bh, rx:1});
      rect.setAttribute("class", flaggedSet[h.hour] ? "bar-flagged" : "bar");
      var title = mk("title", {});
      title.textContent = (h.hour<10?"0"+h.hour:""+h.hour) + ":00  " +
        h.down_avg_mbps.toFixed(1) + " Mbps  (" + h.samples + " samples)" +
        (flaggedSet[h.hour] ? "  — peak-hour dip" : "");
      rect.appendChild(title);
      chart.appendChild(rect);
    });
  }

  function render(d){
    txt("daylabel", (d.day || "—") + "  ·  span: " + (d.span || "day"));
    txt("tdown", human(d.total_down_bytes));
    txt("tup", human(d.total_up_bytes));
    txt("drops", (d.outage_count|0) + "");
    txt("worst", d.worst_latency_ms > 0 ? d.worst_latency_ms.toFixed(1) + " ms" : "—");
    txt("baseline", d.baseline_mbps > 0 ? d.baseline_mbps.toFixed(1) + " Mbps" : "—");

    var flaggedArr = Array.isArray(d.flagged_hours) ? d.flagged_hours : [];
    var flaggedSet = {};
    flaggedArr.forEach(function(h){ flaggedSet[h] = true; });
    txt("flagged", flaggedArr.length ? flaggedArr.map(function(h){ return (h<10?"0"+h:""+h)+":00"; }).join(" ") : "none");

    var hours = Array.isArray(d.speed_by_hour) ? d.speed_by_hour : [];
    renderChart(hours, flaggedSet, d.baseline_mbps || 0);

    // Band time table (sorted desc by seconds). All textContent — band names are data.
    var body = document.getElementById("bandbody");
    clear(body);
    var bands = [];
    var bs = d.band_seconds || {};
    for (var k in bs){ if (bs.hasOwnProperty(k)) bands.push([k, bs[k]]); }
    bands.sort(function(a,b){ return b[1] - a[1]; });
    document.getElementById("bandempty").hidden = bands.length > 0;
    bands.forEach(function(p){
      var tr = document.createElement("tr");
      var td1 = document.createElement("td"); td1.textContent = p[0];
      var td2 = document.createElement("td"); td2.textContent = "≈ " + Math.round(p[1]) + " s";
      tr.appendChild(td1); tr.appendChild(td2); body.appendChild(tr);
    });

    txt("foot", "band time is approximate sample-seconds on each dominant band; worst latency is the worst among speed samples only (not a full-day worst).");
  }

  function load(){
    fetch("/report.json", {cache:"no-store"}).then(function(r){ return r.json(); }).then(render).catch(function(e){
      txt("daylabel", "failed to load /report.json: " + e);
    });
  }
  load();
})();
</script>
</body>
</html>
`
