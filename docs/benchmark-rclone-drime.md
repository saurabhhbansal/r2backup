# r2backup vs rclone on Drime

Measured 27–28 October 2026 on one Windows 11 machine and connection,
against one Drime account. r2backup at this branch; rclone v1.75.0 with its
Drime backend. Both tools ran back to back on identical, generated data, so
both saw the same network. Times are wall-clock for the whole command.

r2backup ran with its defaults. rclone ran `rclone sync <folder> <remote>`
with its defaults (4 transfers), and the first uploads were repeated with
`--transfers 16`, rclone's usual advice for speed.

## Results

### Round 1

| Scenario | r2backup | rclone | rclone `--transfers 16` |
|---|---:|---:|---:|
| First upload, 1,000 files of 2–64 KB in 60 folders | **116 s** | 759 s | 397 s |
| First upload, 2 × 128 MB | 45 s | 46 s | **38 s** |
| Re-run, nothing changed | **1.6 s** | 10.9 s | |
| Re-run after 20 edits, 10 adds, 10 deletes, 1 folder (≈50 files) renamed | 208 s | **85 s** | |
| Re-run after 5 files rewritten at the same size | 41 s | 14 s | |
| Of those 5 same-size edits, how many reached the backup | **5 / 5** | **0 / 5** | |

### Round 2, after two fixes prompted by round 1

Drime was slower during this round — rclone's own no-change run went from
10.9 s to 20.5 s — so compare each row with the rclone figure beside it.

| Scenario | r2backup | rclone |
|---|---:|---:|
| Re-run after 20 edits, 10 adds, 10 deletes, 1 folder renamed | **47 s** | 91 s |
| Re-run after 5 same-size edits | 31.5 s | 22 s (0 / 5 backed up) |
| Re-run, nothing changed | **1.1 s** | 20.5 s |
| First upload, 300 files of 2–64 KB | **135 s** | 557 s |

## What explains the numbers

**Many small files: r2backup about 4–6× faster.** Each small file is one
round trip to Drime, so the number in flight decides the speed. rclone keeps
4 (or 16 when told); r2backup starts at 4 and grows while the server keeps
up, backing off when it answers "slow down" or "unavailable". In the 300-file
trace, 7 % of requests got a 503 from Drime and were retried; none failed.

**Large files: a tie.** Both are limited by Drime's upload bandwidth; both
send 16 MB parts four at a time. rclone with 16 transfers was ~15 % faster
on two files.

**Nothing changed: r2backup 10–20× faster, and it costs Drime nothing.**
r2backup decides what changed from a local index of size and modification
time, so an unchanged folder makes one request (finding the backup folder).
rclone has to list the remote side every run.

**Same-size edits: rclone did not back them up.** Drime keeps no
modification times or hashes, so rclone compares by size alone (its own
docs say so). A file edited without changing size — a database page, a
document with a typo fixed, a photo re-tagged in place — is skipped, and the
run reports success. r2backup compares against its local index, which has
the modification time, and backed up all five. rclone's faster time in that
row is the time it took to do nothing.

**Renamed folder: fixed in round 2.** r2backup recognises a renamed folder
and does not re-upload it. It did that with Drime's copy, which duplicates
the content server-side and took ~17 s per file. It now uses Drime's move,
the same change that sped up its trash. 208 s → 47 s.

## Caveats

- One machine, one connection, one account, two evenings. Drime's own speed
  varied between rounds by about 2×.
- rclone ran with its defaults. `rclone sync --update` would likely have
  caught the same-size edits here: it compares a local file's time with when
  Drime received the copy. rclone's docs note it misses files whose dates are
  set in the past (an extracted archive, a restored file, a camera's clock),
  and it is not the default, so a plain `rclone sync` setup gets the 0 / 5
  above. `--checksum` cannot help: Drime keeps no hashes.
- rclone's `--track-renames` was not used; on Drime it has no hash or
  modification time to match renames by.

## Re-running it

`scripts/bench_drime.py` generates the data and runs round 1; its header says
which environment variables it needs and how to remove the test data
afterwards. It needs Python 3.9+ and rclone with a Drime remote.
