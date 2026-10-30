# Product

## Users

People who back up their own folders (code, documents, photos) to Cloudflare R2 or Drime with `r2b`, on Windows. They set it up once, then mostly forget it. The taskbar panel is how they check on it: a click on the tray icon, a two-second glance at "is everything backed up?", maybe one action (back up now, add a folder, open a folder, turn automatic backups on), then the panel closes. Occasionally something needs them: a folder that could not be read, a run that was interrupted, setup not finished.

## Product Purpose

r2backup keeps folders backed up to cheap object storage, with restore, trash and scheduling. The panel exists so that knowing your backups are fine costs nothing, and fixing them when they are not is one click away. Success: the user trusts it enough to stop checking, and when it does need them, they understand what and why without opening a terminal.

## Brand Personality

Calm, reassuring, plain-spoken. Quiet status at a glance that only raises its voice when something actually needs attention. Copy talks like a careful person ("Your folders are backed up", "Photos needs a look"), never like a dashboard or a marketing page. Confidence comes from restraint and precision, not from decoration.

## Anti-references

- Generic SaaS dashboards: cards on cards, hero metrics, gradient accents, charts for their own sake.
- Electron-web-app feel: a website in a box. The panel should read as a native Windows flyout that belongs next to the clock, not a web page.

## Design Principles

1. Calm by default, loud only when it matters: the healthy state is the quietest; colour and weight are reserved for attention and progress.
2. Answer first: the first thing seen answers "are my backups OK?"; everything else is secondary.
3. Belongs on the desktop: native-feeling density, typography and motion for a Windows flyout, not web-page conventions.
4. Plain words over numbers: say what happened and what to do; show figures only when they help a decision.
5. One step to act: every common action (back up, add a folder, open, schedule) is one click from the panel.

## Accessibility & Inclusion

WCAG 2.2 AA: 4.5:1 text contrast, 3:1 for large text and UI boundaries, in both light and dark themes. Fully keyboard-operable (focus visible, Escape closes). Status is never conveyed by colour alone (icon and words too). Respects prefers-reduced-motion.
