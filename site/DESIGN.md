---
name: r2backup.pages.dev
description: The product page and downloads for r2backup, in a black-and-white Geist system with hairline guides.
colors:
  ink: "#171717"
  ink-hover: "#383838"
  on-ink: "#ffffff"
  ground: "#ffffff"
  ground-2: "#fafafa"
  hover: "#f2f2f2"
  guide: "#ebebeb"
  line: "#e5e5e5"
  line-strong: "#c9c9c9"
  cross: "#a8a8a8"
  muted: "#5c5c5c"
  faint: "#6b6b6b"
  bar-them: "#c2c2c2"
  status-green: "#16a34a"
  dark-ink: "#ededed"
  dark-ink-hover: "#cccccc"
  dark-on-ink: "#0a0a0a"
  dark-ground: "#000000"
  dark-ground-2: "#0a0a0a"
  dark-hover: "#1a1a1a"
  dark-guide: "#1f1f1f"
  dark-line: "#242424"
  dark-line-strong: "#3d3d3d"
  dark-cross: "#5e5e5e"
  dark-muted: "#a1a1a1"
  dark-faint: "#8f8f8f"
  dark-bar-them: "#3d3d3d"
  dark-status-green: "#3ecf6e"
typography:
  display:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(42px, 7vw, 76px)"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "-0.04em"
  headline:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(30px, 3.6vw, 44px)"
    fontWeight: 600
    lineHeight: 1.08
    letterSpacing: "-0.04em"
  closing:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(32px, 4.6vw, 56px)"
    fontWeight: 600
    lineHeight: 1.05
    letterSpacing: "-0.04em"
  title-lg:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "18px"
    fontWeight: 600
    lineHeight: 1.35
    letterSpacing: "-0.015em"
  title:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: 1.35
    letterSpacing: "-0.015em"
  body:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.6
  lede:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(17px, 1.6vw, 19px)"
    fontWeight: 400
    lineHeight: 1.55
  cell:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "15px"
    fontWeight: 400
    lineHeight: 1.6
  tag:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 500
    lineHeight: 1.6
  small:
    fontFamily: "Geist, ui-sans-serif, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.6
  data:
    fontFamily: "Geist Mono, ui-monospace, Consolas, monospace"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.5
rounded:
  bar: "2px"
  code: "5px"
  control: "6px"
  block: "8px"
  panel: "12px"
  pill: "999px"
spacing:
  gutter: "clamp(20px, 4.4vw, 48px)"
  section: "clamp(64px, 9vw, 112px)"
  cell: "28px"
  frame: "1080px"
components:
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.on-ink}"
    rounded: "{rounded.pill}"
    height: "48px"
    padding: "0 22px"
  button-primary-hover:
    backgroundColor: "{colors.ink-hover}"
  button-ghost:
    backgroundColor: "{colors.ground}"
    textColor: "{colors.ink}"
    rounded: "{rounded.pill}"
    height: "48px"
    padding: "0 22px"
  button-ghost-hover:
    backgroundColor: "{colors.hover}"
  button-nav:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.on-ink}"
    rounded: "{rounded.control}"
    height: "32px"
    padding: "0 12px"
  command-block:
    backgroundColor: "{colors.ground-2}"
    textColor: "{colors.ink}"
    typography: "{typography.data}"
    rounded: "{rounded.block}"
  flow-panel:
    backgroundColor: "{colors.ground-2}"
    rounded: "{rounded.panel}"
    padding: "28px"
---

# Design System: r2backup.pages.dev

## Overview

**Creative North Star: "The Drafting Frame"**

The page is a sheet of drafting paper: a 1080px column ruled by 1px guides, with a small "+" wherever a section line meets the frame. Everything sits inside that frame as cells divided by hairlines, never as floating cards. It is pure black or pure white, follows the visitor's system theme, and offers a three-way switch (system, light, dark) in the footer.

The hero is a centred headline over a 3D band: the r2backup block on a line grid, taking in files and sending them out to Drime and S3 storage. The scene is drawn in the page's two inks and its fog fades into the page ground, so it reads as part of the sheet rather than a picture placed on it.

This system is separate from the taskbar panel's (root `DESIGN.md`); the panel keeps Segoe UI and its own tokens.

**Key Characteristics:**
- Two inks and a grey ramp; the only colour is a small green status dot beside the version.
- Geist for every word; Geist Mono only for commands, version numbers and measured values.
- Hairline guides with "+" joints; cells, not cards.
- Real evidence over claims: the rclone benchmark is drawn as bars, the panel as its screenshot.

## Colors

Black and white with a neutral grey ramp (no warm or cool tint). Each token has a light value and a dark value; the dark set applies under `prefers-color-scheme: dark` unless the visitor picked light, and always under `data-theme="dark"`.

### Primary
- **Ink** (#171717 / #ededed): text, the primary pill, r2backup's bars, focus rings and the selection highlight. Hover lifts to #383838 / #cccccc.

### Neutral
- **Ground** (#ffffff / #000000) and **Ground 2** (#fafafa / #0a0a0a): the page and its recessed cells (flow panel, panel screenshot cell, command blocks).
- **Hover** (#f2f2f2 / #1a1a1a): hover fill for ghost buttons, nav links, copy buttons.
- **Guide** (#ebebeb / #1f1f1f): the frame and section rules. **Line** (#e5e5e5 / #242424): borders inside sections. **Line Strong** (#c9c9c9 / #3d3d3d): ghost button outlines, link underlines. **Cross** (#a8a8a8 / #5e5e5e): the "+" joints.
- **Muted** (#5c5c5c / #a1a1a1) and **Faint** (#6b6b6b / #8f8f8f): secondary and tertiary text; both stay above 4.5:1 on Ground and Ground 2.
- **Bar Them** (#c2c2c2 / #3d3d3d): the comparison tool's bars.

### Status
- **Status Green** (#16a34a / #3ecf6e): one 7px dot beside the release version. Nowhere else.

### Named Rules
**The Two Inks Rule.** Ink and ground do all the work. Grey says "secondary"; nothing is coloured for decoration.

## Typography

**Display and body:** Geist (variable, self-hosted from `/fonts`, SIL OFL), with system-ui behind it.
**Data:** Geist Mono, for commands, versions and measured numbers only, with tabular figures.

### Hierarchy
- **Display** (600, 42–76px, line-height 1, -0.04em): the hero headline only.
- **Headline** (600, 30–44px, 1.08, -0.04em): one per section. **Closing** (32–56px) is the last band's headline.
- **Title** (600, 16px, -0.015em): cell and feature titles. **Title Large** (18px): the download cells' OS names.
- **Lede** (400, 17–19px, 1.55): the paragraph under a headline, in Muted.
- **Body** (400, 16px, 1.6), **Cell** (15px, cell and list copy), **Small** (14px, footnotes and links), **Tag** (500, 12px, pill tags).
- **Data** (Geist Mono 13px): commands, version tags, bar values.

### Named Rules
**The No Kicker Rule.** A headline is never introduced by a small label above it. Status such as "Primary backup" rides after a title as a pill tag.

## Layout

A single 1080px column (100% − 32px on small screens) with 1px Guide borders left and right, sections stacked with a Guide rule between them and a "+" joint at each end of the rule. Section padding is `clamp(64px, 9vw, 112px)` with a `clamp(20px, 4.4vw, 48px)` gutter. Grids inside sections bleed to the frame and divide by hairlines (features: 3 × 2, then 2 × 3, then 1 column). The sticky nav is 64px with a Guide bottom rule and a blurred, 82% Ground backdrop so content reads as passing under it.

## Elevation & Depth

Flat. Depth comes from Ground versus Ground 2 and from hairlines. The single shadow is on the panel screenshot: a 1px contact plus a long soft drop (`0 24px 48px -16px` at 18% on light, deeper on dark). The 3D band is the page's only volume.

## Shapes

2px on bar ends and legend swatches; pills (999px) for the main buttons and the theme switch; 6px for nav buttons and copy buttons; 8px for command blocks; 12px for the flow panel, download block, release notes and the screenshot.

## Components

### Buttons
- **Primary** (Download): Ink fill, On-Ink text, pill, 48px in the hero and closing band, 40px in download cells, 500 weight; hovers to Ink Hover.
- **Ghost** (Other platforms, View the source): Ground fill, Line Strong outline, pill; Hover fill on hover.
- **Nav** (GitHub, Download): 32px, 6px radius; GitHub collapses to its icon under 420px.
- **Focus:** 2px Ink outline, 2px offset, on every control.

### Benchmark bars
A real `<table>` laid out as a grid: test name and detail on the left, two stacked 10px bars on the right (r2backup in Ink above, the other tool in Bar Them), value in Geist Mono at the end. Bars are scaled within each row. A range is solid to its low end and hatched to its high end. Bars draw in once, from the left, the first time the table scrolls into view; reduced motion shows them drawn.

### Command block
A Ground 2 box with a 38px header (shell name in Muted, an icon copy button that turns into a check for 1.6 s and announces "Copied" to screen readers) over a single scrolling line of Geist Mono.

### Flow panel
Three cells in a Ground 2 panel, divided by Line rules, each with a 20px stroke glyph, a title (with an optional pill tag) and Muted copy. A round arrow badge sits on each divider; it turns to point down when the cells stack.

### Theme switch
Three 28px round icon buttons (monitor, sun, moon) in a pill outline; the pressed one gets the Hover fill and a Line ring. The choice is kept in `localStorage` and applied before first paint.

### 3D band (signature)
three.js, self-hosted. The logo block (three rounded slabs with a blinking ">_"), a dot floor, and file cards spiralling in and arcing out to two ring markers labelled Drime / Primary and S3 storage / Secondary. Drime-bound files end in Ink, S3-bound files in grey. Every colour is taken from the current theme and swaps live. Follows the pointer and scroll gently; renders one still frame under reduced motion; removed silently without WebGL.

## Do's and Don'ts

### Do:
- **Do** keep everything inside the frame and divide with hairlines.
- **Do** use Geist Mono only where a value is code or a measurement.
- **Do** show evidence (benchmark rows, the real panel) instead of adjectives.
- **Do** check both themes; every token has a pair.

### Don't:
- **Don't** add colour beyond the status dot, gradient text or glows.
- **Don't** float cards with shadows or nest cards.
- **Don't** put a kicker or eyebrow above a headline.
- **Don't** load fonts or scripts from a CDN; everything is served from the site.
