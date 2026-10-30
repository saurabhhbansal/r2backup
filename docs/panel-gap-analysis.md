# Taskbar panel: what still needed a terminal

The taskbar app (`r2bw tray`, `internal/tray/panel.html`) is meant to be the
whole product for a Windows user: nobody should ever have to open a terminal.
This is the audit of every capability the command line (`r2b <command>`) and the
terminal interface (`r2b` with no arguments, `internal/ui`) offer, against what
the panel did after PR #4, and what was built to close the gaps.

Priorities:

- **Must** - without it a normal user ends up stuck, or told to type a command.
- **Should** - a real need, but rare or with a workable path already.
- **Could** - convenience; left out on purpose (see the end).

How often: *daily* (every glance), *sometimes* (a few times a year),
*once* (setup), *rare* (a handful of times ever).

## Matrix

| Capability | Command / terminal key | Panel before | Who, how often | Priority | Built |
|---|---|---|---|---|---|
| See status, progress, last run | `status`, Folders tab | Yes | everyone, daily | - | - |
| Back up now (all / one folder) | `backup [set]`, `b` `B` | Yes | everyone, sometimes | - | - |
| Stop a backup the panel started | `q` on the running screen | No | anyone who started one by mistake, rare | Should | Yes: Stop on the status card |
| Add a folder: name, retention, picker | `add <dir> --name --retention`, `a` | Partly: always everything, folder name forced, no retention, no overlap warning, and a duplicate name failed silently | everyone, once per folder | Must | Yes: add sheet |
| Overlap warning when adding | `add` note, `a` form hint | No | people with nested folders, rare | Must (cost) | Yes |
| Offer automatic backups after the first folder | `add` -> `offerSchedule` | No | new users, once | Must | Yes: switch in the add sheet when the schedule is off |
| Change what is included | `edit`, `e` | Yes | sometimes | - | - |
| Stop backing up / purge | `remove [--purge]`, `x` `X` | Yes | rare | - | - |
| Rename a folder | `rename`, `n` | No | rare | Should | Yes: folder sheet |
| Folder moved: re-point it | `relink`, `m`, prompt after a failed backup | No. The panel showed "no longer exists" and offered nothing | anyone who renames/moves a folder, rare but blocking | Must | Yes: "Find it" in the folder sheet, also from the status card |
| Keep deleted files for N days | `add --retention` | No | sometimes | Must at add time, Should afterwards | Yes: add sheet and folder sheet |
| Restore files / folders | `restore`, `r` | Yes | sometimes | - | - |
| Restore another computer's backup | `restore --machine`, `c` | Yes (grouped by computer) | second computer, once | - | - |
| Check restored files byte for byte | `restore --verify` | No | careful users, rare | Should | Yes: switch in the restore sheet |
| Recover / purge deleted files | `trash ls`, `restore --deleted`, Trash tab | Yes | sometimes | - | - |
| Automatic backups on/off, how often | `schedule [--every] [--remove]`, `s` `e` | Yes | once | - | - |
| Re-point the schedule at a moved program | `schedule --repair`, `p` | No (turning the switch off and on does the same) | rare | Could | No |
| Usage, cost, monthly limit, carry on | `budget set/off/continue`, Cost tab | Yes, inside Settings only | R2 users, sometimes | - | - |
| Paused by the monthly limit, said on the main panel | `status` / Cost tab banner | No: a paused run looked like an ordinary failed run whose text said "To carry on: r2b budget continue" | R2 users with a limit, rare but blocking | Must | Yes: status card + "Carry on" |
| Why did a run fail (full error, counts, examples) | `status` prints them | Truncated one-line text in Activity | anyone with a failure, sometimes | Must | Yes: run detail sheet from Activity |
| A failure that happened before a run started (no storage, index busy) | printed to the terminal | Lost: the hidden child's exit status and error were ignored | rare | Must | Yes: captured and shown as a toast |
| Connect Drime / R2 keys | `setup`, `k` `D` | Yes | once | - | - |
| Cloudflare browser sign-in: find account, pick or create a bucket | `setup` (OAuth) | No: type the account ID and bucket by hand | R2 users, once | Must (the hard part of R2 setup) | Yes |
| Sign in on a second computer, unlock saved keys | `setup`, `i` `d` | Yes | once | - | - |
| Account: who is signed in, devices | `account`, `account devices`, Account tab | No | sometimes | Should | Yes: Account sheet |
| Save storage keys to the account for the next computer | `setup` -> push, `p` | No: `storeVault` existed with no screen, so the "second computer" path could never be set up from the panel | first computer, once | Must | Yes |
| Sign out | `account logout`, `o` | No | rare | Should | Yes (asks twice) |
| Mirror: choose the second destination | `mirror setup` | No screen at all | people who want a second copy, once | Must | Yes |
| Mirror: copy now, every computer's backups | `mirror [--everything]` | No | sometimes | Must | Yes, with progress |
| Mirror after every backup | `mirror auto on/off` | No | once | Must | Yes |
| Mirror: copy back from the mirror | `mirror --reverse` | No | disaster recovery, rare | Should | Yes, without deleting anything, asks twice |
| Mirror: forget the mirror | none (edit files) | No | rare | Could | Yes (asks twice) |
| Mirror dry run / keep | `mirror --dry-run`, `--keep` | No | power users | Could | No |
| Update available / install it | `update [--check]`, `u`, the post-command offer | No: a newer release was only ever mentioned in a terminal | everyone, a few times a year | Must | Yes: daily check, dot on Settings, one-click update; the app restarts itself |
| What is stored, largest first | `ls <set>`, `f` | Partly (restore browser shows sizes) | rare | Could | No |
| First run: connect, first folder, first backup | `setup` then `add` | Partly: no hint for a second computer that already has backups | new users, once | Must | Yes: after connecting, backups already in storage are offered for restore |
| Quit / start with Windows | n/a | Yes | once | - | - |

## Text that pointed at a terminal

Messages from the shared code reach the panel through the Activity list (the
run history's `error`), a folder's note (`sets` status note), call errors and
toasts. These said to run a command or press a key:

| Source | Text | Now in the panel |
|---|---|---|
| `internal/backup` (budget pause) | "To carry on: r2b budget continue" | "Backups are paused: this month reached your spending limit (about $X of $Y)." plus a Carry on button |
| `internal/cli/app.go` connect | "no credentials on this machine yet. Run: r2b setup" | "Connect your storage first: Settings, then Storage." |
| `internal/cli/app.go` resolveSets | "Run: r2b add <folder>" | "Add a folder first." |
| `internal/cli/account.go` | "Run: r2b setup" (signed out / expired) | "Sign in again from Settings, Account." |
| `internal/cli/mirror.go` | "Run: r2b mirror setup" | "Set up a second copy first: Settings, Second copy." |
| `internal/cli/commands.go` remove | "check with: r2b status" | dropped |
| `internal/cli/commands.go` add | "pass --name to choose another" | the add sheet asks for a name before anything runs |
| `internal/cli/dashboard.go` Backup | "Press m on this folder to say where it went" | "Open the folder to find where it went." |
| `internal/cli/dashboard_ops.go` UnlockVault | "Press k to enter your R2 keys instead" | already rewritten by the panel (PR #4) |
| `internal/index` lock | "another copy of r2b is already running (a backup, or the dashboard)..." | "Another backup was running at the same time, so this one did not start. It runs again next time." |
| `internal/drime` manifest | "update with: r2b update" | "Update r2backup: Settings, Update." |
| `internal/backup` root missing | "the folder this set points at no longer exists" | "This folder is not where it was. Nothing was deleted from your backup." |

They are rewritten in one place, `tray.Plain`, which every panel-bound string
goes through (state, call errors, toasts). The command-line wording is left as
it is: in a terminal, a command is the right answer.

## Backend calls added (internal/cli, `trayBackend.Call`)

| Method | Arguments | Does | Reuses |
|---|---|---|---|
| `addCheck` | `path` | suggested name, overlap, whether the folder exists, schedule state | `sets.ValidName`, `d.Overlaps` |
| `addFolder` | `path, name, excludes, retention, schedule` | registers the folder, backs it up, optionally turns the schedule on | `d.Add`, `t.BackUp`, `d.Schedule` |
| `folderList` (extended) | `path, excludes` instead of `name` | browse a folder not added yet | `localTree` |
| `excludeToggle` | `excludes, path, exclude` | the include/leave-out rule as a pure function | `toggleExclude` (shared with `folderExclude`) |
| `folderRename` | `name, to` | rename | `d.Rename` |
| `folderRelink` | `name, path` | point at the folder's new place | `d.Relink` |
| `folderRetention` | `name, days` | keep deleted files for N days | `sets.Update` |
| `folderInfo` (extended) | | retention, note, last run | |
| `runDetail` | `set, at` | one run from the history, in full | `runstate.ReadHistory` |
| `backupStop` | | stops a backup this panel started | the child process |
| `restoreStart` (extended) | `verify` | byte-compares after writing | `restore.Options.Verify` |
| `cfSignIn` / `cfBuckets` / `cfCreateBucket` / `cfDone` / `cfCancel` | | Cloudflare browser sign-in, account and bucket, make a bucket; the token is revoked when done | `oauth.Config`, `cfapi` |
| `accountDevices` | | `account` plus this computer's name | `d.Account` |
| `storeVault`, `signOut` | | (existed) now have a screen | `d.StoreVault`, `d.SignOut` |
| `mirrorInfo` | | destination, automatic on/off, last copied | `a.mirror`, settings |
| `mirrorSetup` | `provider, token / keys, auto` | checks and saves the second destination | `openStore`, `a.mirror.Save`, `setMirrorAuto` |
| `mirrorAuto` | `on` | | `setMirrorAuto` |
| `mirrorRun` / `mirrorStatus` / `mirrorCancel` / `mirrorDismiss` | `everything, reverse` | copies in the background with progress | `mirrorEnds`, `mirror.Run`, `recordMirrored` |
| `mirrorForget` | | removes the second destination | `a.mirror.Delete` |
| `updateCheck` / `updateApply` | | newest release, install it | `d.CheckUpdate`, `d.ApplyUpdate` |
| `resumeBudget` | | (existed) now also on the main panel | |

State additions: `status.action` (what the status card's button does),
`update` (a newer version, from a check made at most once a day),
`noticeAt` (so a toast shows once), and fuller `recent` rows (counts,
examples, the whole error).

## Left out, and why

- **Schedule repair** (`schedule --repair`): turning automatic backups off and
  on in Settings registers the task against the running program, which is the
  same repair. A separate button would be a second way to do one thing.
- **What is stored, largest first** (`ls`): the restore browser already lists
  everything stored with sizes. A cost-oriented "biggest files" view is a nice
  idea, not a gap.
- **Mirror dry run and keep**: power-user flags. The panel's mirror always
  removes from the mirror what was removed from the backup (the default), and
  "copy back" never deletes.
- **Deleting another computer's backups**: the command line cannot do it either
  (only `remove --purge` for this computer's own folders). Restoring them is
  covered.
- **Rename moving the stored prefix**: the command line deliberately does not do
  it (it would cost two operations per file); the panel says the stored name
  stays.
