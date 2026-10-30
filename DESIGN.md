---
name: r2backup taskbar panel
description: The flyout beside the Windows clock that says whether your backups are fine.
colors:
  signal: "#ececf0"
  signal-ink: "#121214"
  ground: "#121214"
  surface: "#1b1b1f"
  surface-raised: "#232328"
  hairline: "#2c2c32"
  ink: "#ececf0"
  ink-muted: "#8d8d97"
  ink-faint: "#5c5c66"
  healthy-green: "#34d399"
  pending-amber: "#fbbf24"
  alarm-red: "#f87171"
  light-signal: "#18181b"
  light-signal-ink: "#ffffff"
  light-ground: "#f6f6f8"
  light-surface: "#ffffff"
  light-surface-raised: "#f0f0f3"
  light-hairline: "#e3e3e8"
  light-ink: "#18181b"
  light-ink-muted: "#6b6b75"
  light-healthy-green: "#059669"
  light-pending-amber: "#d97706"
  light-alarm-red: "#dc2626"
typography:
  title:
    fontFamily: "Segoe UI Variable Text, Segoe UI, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 600
    lineHeight: 1.4
  body:
    fontFamily: "Segoe UI Variable Text, Segoe UI, system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.4
  body-strong:
    fontFamily: "Segoe UI Variable Text, Segoe UI, system-ui, sans-serif"
    fontSize: "13px"
    fontWeight: 500
    lineHeight: 1.4
  label:
    fontFamily: "Segoe UI Variable Text, Segoe UI, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: 1.4
  caption:
    fontFamily: "Segoe UI Variable Text, Segoe UI, system-ui, sans-serif"
    fontSize: "11px"
    fontWeight: 400
    lineHeight: 1.4
rounded:
  sm: "7px"
  md: "9px"
  window: "10px"
  card: "12px"
  pill: "999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "10px"
  lg: "14px"
components:
  button-primary:
    backgroundColor: "{colors.signal}"
    textColor: "{colors.signal-ink}"
    rounded: "{rounded.md}"
    padding: "0 14px"
    height: "34px"
  button-mini:
    backgroundColor: "{colors.surface-raised}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
    padding: "0 9px"
    height: "26px"
  button-mini-hover:
    backgroundColor: "{colors.hairline}"
  status-card:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.card}"
    padding: "12px"
  folder-row-hover:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.md}"
    padding: "9px 8px"
  settings-group:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.card}"
  toggle-on:
    backgroundColor: "{colors.signal}"
    rounded: "{rounded.pill}"
    width: "38px"
    height: "22px"
---

# Design System: r2backup taskbar panel

## Overview

**Creative North Star: "The Quiet Sentinel"**

The panel keeps watch without fuss. A 380 by 580 flyout beside the Windows clock, it exists to answer one question at a glance, "are my backups fine?", and to get out of the way. When everything is healthy it is the calmest thing on the desktop: near-black ground, soft grey copy, one small green tick. It speaks up plainly, in words and in colour, only when something needs the person: a folder that could not be read, a run that stopped, setup not finished.

It should feel like it belongs to Windows, not like a website in a box. Segoe UI Variable, compact density, rounded Windows 11 corners, keyboard and Escape behaviour of a real flyout. The direction for its components is soft and tactile: surfaces that feel gently lifted and pressable, comfortable hit targets, rounded corners, a little more air than a dense developer tool, never the cards-on-cards of a SaaS dashboard.

**Key Characteristics:**
- One answer first: the status block leads; everything else is secondary.
- Calm by default; colour is earned by state (green healthy, amber waiting, red needs you; progress is plain ink).
- Native Windows flyout behaviour and type; dark and light follow the system theme.
- Soft, tactile, rounded controls with comfortable targets.

## Colors

A monochrome world: near-black (or near-white in light mode) neutrals, with ink itself as the accent, and three quiet status colours that appear only when there is something to say. The logo, the tray icon and the program icon are black or white too.

### Primary
- **Signal** (ink: #ececf0 on dark, #18181b on light, with #121214 / #ffffff on top of it): progress and action. The primary "Back up now" button, the progress bar, switches that are on, the selected tab underline, focus rings. It means "something is happening or can happen here", never decoration.

### Neutral
- **Ground** (#121214 / #f6f6f8): the window background.
- **Surface** (#1b1b1f / #ffffff): the status block, settings groups, row hover.
- **Raised Surface** (#232328 / #f0f0f3): small buttons, the toast, progress track.
- **Hairline** (#2c2c32 / #e3e3e8): the window border, dividers, group borders.
- **Ink** (#ececf0 / #18181b), **Muted Ink** (#8d8d97 / #6b6b75), **Faint Ink** (#5c5c66): primary text, secondary text, tertiary metadata.

### Status
- **Healthy Green** (#34d399 / #059669): backed up, automatic backups on.
- **Pending Amber** (#fbbf24 / #d97706): not yet run, waiting, setup unfinished, a stopped run.
- **Alarm Red** (#f87171 / #dc2626): a folder needs a look, a failed run, Quit.

### Named Rules
**The Earned Colour Rule.** Colour reports state; it is never ornament. The healthy panel shows almost no colour beyond one green mark; the primary button is plain ink.

**The Never Colour Alone Rule.** Every status colour travels with an icon and words.

## Typography

**Body Font:** Segoe UI Variable Text (with Segoe UI, system-ui)

**Character:** One family, the one Windows itself uses, in three weights. Native, quiet, legible at small sizes.

### Hierarchy
- **Title** (600, 14px, 1.4): the status line, the panel name, empty-state headings.
- **Body** (400, 13px, 1.4): default text, tab labels (500), folder names (500).
- **Label** (400, 12px, 1.4): secondary lines, paths, last-run notes, small buttons.
- **Caption** (400, 11px): the version footnote only.

## Layout

A fixed 380 by 580 window, stacked top to bottom: header (the stack logo in text colour, name, computer name, three icon buttons), status block, tabs (Folders / Activity), a scrolling list (the empty and setup states lead with the wide logo), and a footer with the full-width primary action. Everything is done in the panel; nothing sends a person to a terminal. Settings, connecting storage, restoring files, deleted files, a folder's settings and usage and cost each slide in over the whole window as a sheet from the right, stacked in the order they were opened, and Escape or the back arrow closes the top one. Horizontal gutter 14px; list rows inset 8px. Rows are a three-column grid: status icon, name and detail, trailing metadata that swaps to row actions on hover or focus.

## Elevation & Depth

Depth is tonal today: Ground, then Surface, then Raised Surface, separated by 1px hairlines; the only shadow is the Windows window shadow. The tactile direction allows soft, low lift on interactive surfaces (buttons, the status block) where it makes them feel pressable, never stacked shadows or glass.

## Shapes

Windows 11 roundness throughout: 10px window corners, 12px for the status block and settings groups, 9px for rows and the primary button, 7px for small buttons, full pills for switches and the status dot. Circles for status badges.

## Components

### Buttons
- **Primary** (Back up now): Signal fill, inverse ink text, 9px radius, 34px tall, icon plus label; brightens slightly on hover; half opacity when disabled.
- **Mini** (row Open / Back up, empty-state actions): Raised Surface fill, 7px radius, 26px tall, 12px text; Hairline on hover.
- **Icon buttons** (header): 32px square, 8px radius, Muted Ink icon, Raised Surface and Ink on hover.
- **Focus:** 2px Signal outline, 2px offset, on every control.

### Status block
- Surface fill, 12px radius, hairline border, a 34px circular badge tinted with the state colour (14% fill), title and one-line detail, and a 4px progress bar while running (indeterminate slide until a percentage is known).

### Folder and activity rows
- Transparent until hover, then Surface. Status icon in the state colour, name in Body 500, path or note in Label Muted (Alarm Red for a problem), trailing last-run in Faint; on hover or focus the trailing text swaps for Open and Back up.

### Settings sheet
- Grouped rows inside Surface groups with hairline dividers; switches 38 by 22 pills, Signal when on; the Quit row in Alarm Red.

### Sheets and forms
- A sheet is a back arrow and a title over a scrolling body, with an optional footer holding one action. Steps inside a sheet (choose storage, then enter a token) swap in place and the back arrow walks them back.
- Fields: Surface fill, hairline border, 8px radius, a Signal outline on focus, the label above in Muted Ink. Errors sit under the field in Alarm Red, in plain words, and the action that failed stays pressable.
- A button that waits on something says what it is doing ("Checking…") and puts itself back.
- Destroying anything asks twice: a second press within a few seconds, or typing the folder's name for the permanent kind. Plain-ink switches mean "backed up" / "on"; a half-state shows as a half-slid switch.

### Toast
- Raised Surface pill-ish bar above the footer, slides up 6px and fades in, gone after about three seconds.

## Do's and Don'ts

### Do:
- **Do** lead with the status answer and keep the healthy state the quietest state.
- **Do** use Signal only for progress, the primary action, on-switches, selection and focus.
- **Do** keep every status readable without colour (icon plus words).
- **Do** keep text at 4.5:1 or better against its surface in both themes.
- **Do** follow the system theme and keep motion short, with a reduced-motion fallback.

### Don't:
- **Don't** build a SaaS dashboard: no cards on cards, hero metrics, gradient accents or charts for their own sake.
- **Don't** make it feel like a website in a box: no web-page scrolling heroes, link-styled navigation or browser-looking chrome.
- **Don't** use colour as decoration in the healthy state.
