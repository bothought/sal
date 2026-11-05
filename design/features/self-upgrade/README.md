# Self-upgrade

This item covers one capability: sal replacing its own binary with a newer
release downloaded from GitHub. It describes the two commands the user types,
what they print, what they change on disk, and how they behave when something
goes wrong. How the download, verification and replacement are built is
`design/technical/self-upgrade`; the release assets it consumes are produced by
the release pipeline described in the same document.

## 1. Purpose

sal is installed as one static binary, and today the only ways to get a newer
version are `go install`, a rebuild from a checkout, or a manual download. Each
of those requires knowing how sal was installed and having a toolchain or a
browser at hand. A person who copied the binary somewhere on `PATH` has no
in-place way to update it, and no way to even find out whether a newer release
exists short of visiting the releases page.

The feature gives the user:

- `sal upgrade --check`, which reports the installed version, the newest
  published release, and whether an update is available — and changes nothing;
- `sal upgrade`, which downloads the newest release for the machine it runs on,
  verifies it, and replaces the running binary with it;
- a clear, non-destructive failure in every case where the replacement cannot be
  done safely, with the working binary left exactly as it was.

## 2. Vocabulary

- **Installed version** — the version of the binary that is running; printed by
  `sal --version`.
- **Latest release** — the most recent published, non-draft, non-prerelease
  release of the project repository on GitHub.
- **Release asset** — one downloadable file attached to that release: an archive
  of the binary for one operating system and architecture, or the checksum list.
- **Upgrade** — replacing the running binary's file with the binary from the
  latest release.
- **Release version** — the version a release's tag names (`v0.2.0`), written
  without the leading `v` when compared or displayed beside the installed
  version.

## 3. The commands

```
sal upgrade [--check] [--help]
```

`upgrade` is a reserved first word on the command line, not a flag. Because sal
otherwise treats every positional argument as part of the prompt, `sal upgrade`
with no further arguments is the subcommand; a prompt that begins with that word
is written `sal -- upgrade ...`. The reserved word is documented in `--help` and
in the README.

The subcommand is self-contained: it needs no API key, does not load the
configuration file, does not read piped input, does not start a session, and
never talks to a model. It does not appear as a tool the model can call.

### 3.1 `sal upgrade --check`

Reports whether a newer release exists and exits without touching any file:

```
$ sal upgrade --check
sal 0.1.0
latest release: 0.2.0
an update is available: 0.1.0 → 0.2.0
  run 'sal upgrade' to install it
```

- The installed version is printed even when it cannot be compared (see §3.5).
- When the installed version is the latest: `sal 0.2.0 is up to date`, followed
  by `latest release: 0.2.0`.
- When the installed version is newer than the latest release (a local build, a
  release that was withdrawn): `sal 0.3.0 is newer than the latest release
  (0.2.0); nothing to do`. It is never a suggestion to downgrade.
- `--check` never prompts, never writes, and never creates temporary files.

### 3.2 `sal upgrade`

With an update available:

```
$ sal upgrade
sal 0.1.0 → 0.2.0 (linux/arm64)
downloading sal_0.2.0_linux_arm64.tar.gz (11.4 MiB) ... done
verifying checksum ... ok
replacing /home/me/.local/bin/sal ... ok
sal upgraded to 0.2.0; start sal again to use the new binary
```

The second line of the last line is intentional: the process that performed the
upgrade keeps running the old binary, and only a new process uses the new one.

- The platform fragment is the one the asset was chosen for (`linux/arm64`,
  `darwin/arm64`, `windows/amd64`), not necessarily the machine's raw
  `GOOS`/`GOARCH`; see §3.7.
- When there is nothing to do, the output is the same three lines as
  `--check` and the exit status is 0.
- The command does not ask for confirmation. Typing `sal upgrade` is the
  confirmation; there is no `--yes` and no interactive prompt, so it works in a
  script, over SSH, and with no terminal attached.

### 3.3 Exit status

Both forms exit 0 when they completed their job: `--check` found or did not find
an update, `upgrade` installed the latest release or determined there was
nothing to install. Any failure — network, asset, checksum, replacement —
exits 1 with a single `sal: <what failed>` line on standard error. A check is not
an error condition, so scripts must read the output rather than the exit status
to learn whether an update exists.

### 3.4 `sal upgrade --help`

Prints the subcommand's usage, the platform it would upgrade for, and the two
behaviours above. Like the rest of `--help`, it is available without a
configuration file or a network connection.

### 3.5 Which version is installed

The installed version is the one `sal --version` prints:

- a release binary reports the version of the release it was built from;
- a binary installed with `go install ...@latest` reports the module version the
  Go toolchain recorded;
- any other build (a checkout built with `go build`, a local development build)
  reports `dev`, including a build whose version the toolchain derived from a
  checkout rather than from a published release.

`dev` cannot be ordered against a release, so `--check` reports the latest
release and says `installed version 'dev' cannot be compared; run 'sal upgrade'
to install the latest release`, and `upgrade` installs the latest release
without a version comparison. `dev` is never treated as newer or older.

Versions are compared as semantic versions (`major.minor.patch`, optional
`-prerelease`). A release that is not a semantic version is reported as such and
cannot be upgraded to; the checksum list and the asset rules are unchanged.

### 3.6 Which release is used

The latest published release of the project repository on GitHub, excluding
drafts and prereleases. There is no `--version`/`--tag` argument: selecting an
older or specific release is not part of this feature, and a downgrade cannot be
requested.

### 3.7 Which asset is used

The release carries one archive per operating system and architecture:

| Machine | Asset |
|---|---|
| Linux `amd64`, `arm64` | `sal_<version>_linux_<arch>.tar.gz` |
| macOS `amd64`, `arm64` | `sal_<version>_darwin_<arch>.tar.gz` |
| Windows `amd64`, `arm64` | `sal_<version>_windows_<arch>.zip` |
| Android/Termux `arm64` | the `linux/arm64` archive |

The asset is chosen by matching the release's asset names, so a release whose
assets follow that naming is usable by any installed version that knows the
platform rule. A machine whose platform has no asset in the release is told so
by name (`no release asset for plan9/arm64`) and nothing is downloaded.

### 3.8 What is checked before the binary is replaced

An upgrade is a sequence of checks, and any of them failing stops the upgrade
with the installed binary untouched:

1. The running binary's file can be located.
2. The release has exactly one asset for this platform, and a checksum list
   that mentions it.
3. Everything is fetched over HTTPS; a redirect to a non-HTTPS URL aborts the
   download.
4. The download is bounded in size and in time; exceeding either aborts it.
5. The asset's SHA-256 matches the release's checksum list.
6. The archive contains the binary for this platform.
7. The extracted binary runs and reports the expected version.
8. The target location accepts a new file (it is writable, and the filesystem
   allows executing what is written there).

Only then is the file replaced, and the replacement is atomic: a process that
fails, is killed, or loses power during it leaves either the old binary or the
new one, never a partial file. Temporary files created along the way live in the
target's directory and are removed on every exit path, including an interrupt.

### 3.9 What is replaced, and what is not

- The file is the one that was executed. If `sal` on `PATH` is a symbolic link,
  the link is left in place and the file it points to is replaced, so an upgrade
  does not change which name the binary is invoked by.
- Nothing else is touched. Settings, sessions, the web-console token, and the
  working directory are untouched, and no lock file is written.
- A file installed by a package manager is still replaced if the location is
  writable; sal does not detect or defer to package managers. Where the
  directory is not writable, the upgrade fails with a message saying so and
  naming the directory, rather than trying to elevate privileges.
- A binary inside the Go module cache (the result of `go install`) is replaced,
  but the module cache is not updated, so `go install` would install the old
  version again. The README says so and points those installations at
  `go install ...@latest` instead.

### 3.10 Windows

Windows cannot overwrite or delete a running executable, so the upgrade renames
the running file out of the way, puts the new file in its place, and leaves the
old file for deletion. The next time sal starts on Windows it removes leftover
files from earlier upgrades (best effort, silently). Every other rule — the
checks, the atomicity, the messages — is the same as on the other platforms.

## 4. Failure behaviour

Each failure prints one `sal: ...` line naming what failed and exits 1; the
installed binary keeps working. The messages are stable enough to search for:

| Situation | Message names |
|---|---|
| No network, DNS failure, TLS failure | that the release could not be reached, with the underlying error |
| GitHub rate limit (HTTP 403 or 429) | the rate limit and the suggested wait |
| Repository or release missing (404) | that no release was found |
| No asset for this platform | the platform, and that the release has no asset for it |
| Checksum file missing, or without this asset | which of the two |
| Checksum mismatch | that the download failed verification, and that nothing was replaced |
| Archive does not contain the binary | what was found instead |
| Extracted binary does not run, or reports the wrong version | that the download was rejected, and that nothing was replaced |
| Target directory not writable | the directory, and that the binary was left unchanged |
| Target read-only or on a `noexec` filesystem | the target path |
| Interrupted (Ctrl-C) | nothing further is printed; temporary files are removed |

A failed upgrade never leaves a partially written binary behind and never
deletes the installed one. Running the command again is always safe.

## 5. What the upgrade does not do

Deliberately out of scope:

- Downgrades, or installing a pinned or prerelease version.
- Verifying a signature: the checksum list and the asset come from the same
  release, so a compromised repository can serve both. The download is protected
  against corruption and against a truncated or swapped-in file, not against a
  compromised publisher.
- Updating anything besides the binary: no session migration, no configuration
  edits, no re-running of any startup step.
- Network mirrors, proxies configured inside sal, or credentials for the GitHub
  API: only the standard system proxy environment is honored.
- Being callable by the model as a tool, or reachable from the terminal UI and
  the web console as a slash command.
- Running as a background or scheduled check: `--check` exists to be called by
  the user or by a script.
