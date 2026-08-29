<!--
The standard shape for a release's notes. Copy this to
.github/release-notes/<tag>.md — for example v1.0.1.md — and fill it in as
part of the change, not afterwards. The release workflow passes the file for the tag being built to goreleaser
with --release-header. On the published page it lands under the short product
blurb from .goreleaser.yaml and above the generated commit changelog — so
write it as the body of the release, not as its opening line.

Rules, so this never has to be decided again:
  * Write for someone who has the previous version installed and wants to
    know whether to bother. Not for someone reading the diff.
  * "New" is a thing they can now do. "Fixed" is something that used to be
    wrong for them. Anything that is neither does not go in.
  * Name the thing, then what it means. "Everything is in the interface now"
    is a claim; "sign in, add a folder and restore without leaving the
    window" is the same claim a reader can check.
  * If an upgrade needs a manual step, it goes at the top under Heads up,
    never buried at the bottom.
  * No section with nothing in it — delete the heading instead.
-->

## What's new

- **Short name for it.** One or two sentences on what you can now do.

## Fixed

- **What was wrong.** What happened before, and what happens now.

## Heads up

- Anything that needs the reader to do something by hand.
