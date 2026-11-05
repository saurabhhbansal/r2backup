# Contributing to r2backup

Thanks for looking. r2backup backs up folders to Drime (Primary backup), with an optional S3 copy on R2, B2 or any S3 storage (Secondary backup). A good change is small, tested, and makes the program calmer or more reliable for someone who set it up once and wants to forget it.

If you are new to the code, read [docs/architecture.md](docs/architecture.md) first. [PRODUCT.md](PRODUCT.md) says who this is for, and [DESIGN.md](DESIGN.md) covers the look of the taskbar panel.

## What is welcome

- Bug fixes, especially ones with a test that fails before and passes after.
- Better error messages and plainer wording.
- Support for another S3 provider, or fixes for one that behaves differently.
- macOS and Linux polish for the `r2b` command line.
- Tests, docs and typo fixes.

For anything bigger than a small fix (a new command, a new storage backend, a change to how backups are laid out), open an issue first so we can agree on the shape before you spend time on it. Use the [issue templates](.github/ISSUE_TEMPLATE). Security problems do not go in public issues: see [SECURITY.md](SECURITY.md).

## Setting up

You need:

- Go 1.25 or newer (the version in `go.mod`; CI uses 1.25).
- Node 22 and npm, only if you touch `worker/` or `relay/`.
- Git. Windows, macOS or Linux all work for the Go code. The taskbar app (the panel) only runs on Windows, but everything else builds and tests anywhere.

```
git clone https://github.com/saurabhhbansal/r2backup
cd r2backup
go build ./...
```

Build the two programs:

```
go build -o r2b ./cmd/r2b        # the command line and terminal app
go build -o r2bw ./cmd/r2bw      # Windows only: runs r2b with no console window
```

Releases are built by GoReleaser (`.goreleaser.yaml`); you do not need it locally.

## Running tests

CI runs these, so run them before you open a pull request:

```
go vet ./...
go test ./... -race -count=1 -timeout 20m
```

The second command includes the end-to-end tests, which take several minutes. While you work you can run one package, or the e2e suite on its own:

```
go test ./internal/engine
go test ./test/e2e -count=1 -timeout 20m
```

The integration tests talk to a real MinIO server. MinIO no longer publishes binaries, so the harness in `test/minio` builds a pinned release from source the first time (cached in `~/.cache/r2backup-test`). To prebuild it the way CI does:

```
go test ./test/minio -run TestBinary -count=1 -timeout 20m
```

CI also sets `R2BACKUP_REQUIRE_FULL_SUITE=1`, which turns a skipped test into a failure. You do not need it locally. `test/r2` runs against real Cloudflare R2 and only when the `R2BACKUP_TEST_*` variables are set; leave it alone unless you have a disposable bucket.

The two Workers are Node projects:

```
cd relay  && npm ci && npm test      # Ultra fast mode relay; run by CI
cd worker && npm ci && npm test      # account service; not run by CI, so run it yourself
cd worker && npm run typecheck
```

### Formatting

CI fails if `gofmt -l .` prints anything. On a Windows checkout with CRLF line endings, plain `gofmt -l` lists every file. Check the committed (LF) version instead:

```
git show HEAD:internal/cli/tray.go | gofmt -l
tr -d '\r' < internal/cli/tray.go | gofmt -l
```

No output means formatted. Do not reformat whole files just to change line endings.

## Things that will bite you

- **Never test against your real accounts or backups.** `r2b` reads and writes your real data directory by default. Point it somewhere disposable first: `R2BACKUP_DATA_DIR=/some/scratch/dir r2b ...` (PowerShell: `$env:R2BACKUP_DATA_DIR = "C:\scratch"`).
- **Tests must not touch the OS scheduler.** The tests in `internal/cli` replace the scheduler with stubs (`scheduleInstall` and `scheduleRemove` in `internal/cli/main_test.go`), and that file's `TestMain` forces a temporary data directory. A test that registers a real Task Scheduler, launchd or systemd entry leaves it behind, pointing at a deleted binary. New tests should use the same seams. The one exception is CI's `R2BACKUP_TEST_SCHEDULER=1`, which tries the real scheduler on a throwaway runner; do not set it on your own machine.
- **Every command and flag needs a key in the terminal app.** `r2b` with no arguments opens a full-screen interface, and anything the command line can do, that interface can do. `TestEveryCommandAndFlagIsReachableFromTheInterface` in `internal/cli/coverage_test.go` walks the real command tree and fails if a command or flag has no entry in its `tuiSurface` table. Adding a flag means adding a line there, and that is the moment to notice there is nowhere to press it. A flag that makes no sense in an interactive window can be marked not applicable, with a sentence saying why. `TestEveryCommandIsReachableAndDocumented` in `internal/cli/root_test.go` checks the help text.
- **The panel and the terminal share code.** The panel (`internal/tray/panel.html`) has no backup logic of its own; its buttons call into `internal/cli` (see the architecture doc). If you add a panel feature, add the matching command and key.

## Previewing the panel

You can click through every panel screen in an ordinary browser, on any OS, with no real storage and a fake account service:

```
R2B_PANEL_PREVIEW=127.0.0.1:8765 go test ./internal/cli -run TestPanelPreview -timeout 0
```

Then open the address you gave it. The email sign-in code is `123456`. It uses a scratch data directory and a fake Cloudflare, and keeps running until you stop it. The real Windows window (WebView2) is in `internal/tray/tray_windows.go` and needs Windows to try.

## Wording

Text people read should be plain and short. Say what happened and what to do, not what the program did internally: "Photos needs a look", not "Scan error in set 2". Use "Primary backup" and "Secondary backup" for the two places data goes, and "S3 storage" for R2, B2 and the like. [PRODUCT.md](PRODUCT.md) describes the tone.

## Pull requests

1. Branch from `main`. One idea per pull request; small is easier to review and easier to undo.
2. Add or update tests. A fix without a test that would have caught it is half done.
3. Run `go vet ./...`, the formatting check, and the tests for what you touched. CI runs the full suite on Linux, Windows and macOS for pull requests into `main`.
4. If a user would notice the change, add a note to the release notes for the next version: `.github/release-notes/<next tag>.md` (for example `v1.5.1.md`), following [`.github/release-notes/TEMPLATE.md`](.github/release-notes/TEMPLATE.md). Write for someone with the last version installed who wants to know whether to bother. If the file for the next tag already exists, add to it.
5. Update the docs that mention what you changed.
6. Fill in the pull request template.

### Commit messages

Look at `git log --oneline`. Recent subjects are one plain sentence saying what is true now, in sentence case, with no `feat:` or `fix:` prefix and no trailing full stop. Pull requests are squash-merged and the number is added at the end:

```
Remove usage and cost; r2b setup can leave the second copy for later (#18)
Renaming back to a long hostname no longer fails (#10)
Show the Ultra fast mode switch where people look (#21)
```

Older commits used `fix(cli):` style prefixes; do not copy those. Put the reason in the body when the diff does not make it obvious.

## Licence

By contributing you agree that your work is released under the repository's [LICENSE](LICENSE).
