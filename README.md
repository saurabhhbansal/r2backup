# r2backup

Back up folders to Cloudflare R2 and restore them anywhere. One static binary —
no installer, no runtime, no background service. The operating system's own
scheduler runs it, and it exits.

- **Mirror, not snapshots.** The bucket holds your folder as it is now, one
  object per file, browsable in the R2 dashboard.
- **Deleted and overwritten files stay recoverable for 30 days** in trash.
- **A run that changes nothing costs nothing.** A local index decides what
  changed, so an unchanged tree makes no requests at all and stays inside R2's
  free tier.
- **An honest ETA.** If it says two hours, two hours is what it means.

## Install

**Linux / macOS**

```sh
curl -sSL https://github.com/saurabhhbansal/r2backup/releases/latest/download/install.sh | sh
```

**Windows** (PowerShell)

```powershell
irm https://github.com/saurabhhbansal/r2backup/releases/latest/download/install.ps1 | iex
```

Both verify the download against the published checksums before installing.
Or take the archive for your platform from the
[releases page](https://github.com/saurabhhbansal/r2backup/releases) and put
`r2backup` on your PATH.

## Getting started

You need a Cloudflare R2 bucket and an S3 API token for it (Cloudflare
dashboard → R2 → Manage API tokens).

```sh
r2backup setup                  # store your R2 credentials on this machine
r2backup add ~/Documents        # pick what to include, then back it up
r2backup schedule --every 30    # run it every 30 minutes from now on
r2backup status                 # what ran, when, and what is next
```

`add` opens the folder as a tree with everything already selected. Uncheck what
you do not want and press enter — or press enter straight away and all of it
goes. It is the only place the tool asks you anything. `--all` skips it.

To bring a folder back:

```sh
r2backup restore Documents              # to where it came from
r2backup restore Documents --to ~/tmp   # somewhere else
r2backup restore Documents --verify     # re-read and byte-compare every file
```

Files that are already there are left alone and counted; pass `--overwrite` to
replace them. Restoring onto a different computer works the same way, except
that `--to` is required, because it will not guess a path that does not exist.

## On another computer

Credentials can travel, encrypted with a passphrase only you hold. On the
machine that already works:

```sh
r2backup login you@example.com  # a six-digit code arrives by email
r2backup account push           # encrypt its R2 keys under a passphrase you choose
```

Then on the new machine, the same `r2backup login you@example.com` signs in and
pulls those credentials down. `r2backup account devices` lists which computers
have signed in, `r2backup account logout` forgets this one.

The server stores a blob it cannot read. Forgetting the passphrase is not a
data-loss event: it guards the stored credentials, not your files, which are
never encrypted client-side. The worst case is typing your R2 keys in again.

## Commands

| | |
|---|---|
| `setup` | Store this machine's R2 credentials |
| `add <folder>` | Pick what to include, then back it up |
| `backup [set]` | Back up now, all sets or one |
| `restore <set>` | Bring a folder back |
| `ls [set]` | What is in the backup |
| `trash ls [set]` | What is recoverable, and until when |
| `status` | What ran, when, and what is next (`--watch` follows a run) |
| `schedule` | Register with the OS scheduler (`--remove` to unregister) |
| `rename <set> <name>` | Change what a set is called |
| `relink <set> <path>` | Point a set at a folder that moved |
| `remove <set>` | Stop backing up a folder (`--purge` also deletes the backup) |
| `account`, `login` | Share credentials with your other computers |
| `update` | Replace this binary with the latest release |

`--yes` and `--no` answer any prompt for you, for scripts and scheduled runs.

## Where things live

| | |
|---|---|
| Windows | `%LOCALAPPDATA%\r2backup` |
| macOS | `~/Library/Application Support/r2backup` |
| Linux | `$XDG_DATA_HOME/r2backup`, else `~/.local/share/r2backup` |

Your R2 credentials sit alongside them in a `credentials` file. On Windows the
secret is encrypted with DPAPI, so it is readable only by your account on that
machine. On macOS and Linux there is no keystore backend yet and the file is
0600 in a 0700 directory and nothing more — `setup` says which of the two you
got rather than implying encryption that is not happening.

If a backed-up folder is renamed or moved, r2backup stops rather than reading
the missing folder as a deletion. Run it yourself and it asks where the folder
went, repoints it and carries on; a scheduled run leaves it for you and says so
in `status`. Nothing is re-uploaded either way.

`remove` stops backing a folder up and leaves what is already in the bucket
alone; `remove --purge` deletes those objects too, permanently. Adding the
folder again reaches the same place in the bucket, but uploads it once more —
the record of what is already there is kept on this computer, and it goes with
the set.

## Licence

MIT. See [LICENSE](LICENSE).
