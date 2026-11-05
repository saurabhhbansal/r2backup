## What this changes

<!-- One or two plain sentences: what is true now that was not before, and why. -->

## Checklist

- [ ] Tests added or updated, and `go test ./...` passes for what I touched
- [ ] `go vet ./...` is clean and the Go files are gofmt'd (see CONTRIBUTING.md for the CRLF caveat on Windows)
- [ ] New or changed commands and flags have a key in the terminal app (`tuiSurface` in `internal/cli/coverage_test.go`)
- [ ] Release note added in `.github/release-notes/<next tag>.md` if a user would notice
- [ ] Docs updated (README, CONTRIBUTING, `docs/`) where they mention this
- [ ] Tried against a disposable data directory (`R2BACKUP_DATA_DIR`), not my real backups
