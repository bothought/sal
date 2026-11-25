# Self-upgrade

`sal upgrade` replaces the installed binary with the latest release from GitHub.
`sal upgrade --check` reports whether a newer release exists and changes nothing.
How it is built is `design/technical/self-upgrade`.

## 1. Commands

```
sal upgrade [--check] [--help]
```

`upgrade` is a reserved first word. A prompt that begins with it is written
`sal -- upgrade ...`. The subcommand needs no API key, configuration file,
session, or terminal, and never talks to a model.

### 1.1 `sal upgrade --check`

```
$ sal upgrade --check
sal 0.1.0
latest release: 0.2.0
an update is available: 0.1.0 → 0.2.0
  run 'sal upgrade' to install it
```

- Up to date: `sal 0.2.0 is up to date`, then `latest release: 0.2.0`.
- Installed version newer than the latest release: `sal 0.3.0 is newer than the
  latest release (0.2.0); nothing to do`.
- Unorderable version (`dev`): reports the latest release and says the installed
  version cannot be compared.
- `--check` never prompts, writes, or creates temporary files.

### 1.2 `sal upgrade`

```
$ sal upgrade
sal 0.1.0 → 0.2.0 (linux/arm64)
downloading sal_0.2.0_linux_arm64.tar.gz (11.4 MiB) ... done
verifying checksum ... ok
replacing /home/me/.local/bin/sal ... ok
sal upgraded to 0.2.0; start sal again to use the new binary
```

The running process keeps the old binary; only a new process uses the new one.
When there is nothing to install, the output matches `--check`. The command asks
for no confirmation and works without a terminal.

Both forms exit 0 when they completed their job. A failure exits 1 with one
`sal: <what failed>` line on stderr and leaves the installed binary working.

## 2. Versions

The installed version is what `sal --version` prints:

- a release build reports the release tag;
- a `go install ...@latest` build reports the module version;
- any other build reports `dev`.

`dev` cannot be ordered against a release; `upgrade` installs the latest release
without a comparison. Versions are semantic versions with an optional
prerelease.

Only the latest published non-draft, non-prerelease release is used. There is no
way to choose a version or downgrade.

## 3. Platforms and assets

| Machine | Asset |
|---|---|
| Linux `amd64`, `arm64` | `sal_<version>_linux_<arch>.tar.gz` |
| macOS `amd64`, `arm64` | `sal_<version>_darwin_<arch>.tar.gz` |
| Windows `amd64`, `arm64` | `sal_<version>_windows_<arch>.zip` |
| Android/Termux `arm64` | the `linux/arm64` archive |

The release must have exactly one asset matching the platform and a checksum list
that names it. A missing or ambiguous match is an error, not a guess.

Android serves DNS through a system daemon and Termux keeps its CA bundle under
its own prefix. When the system files are absent, sal reads the platform's
resolver configuration and certificate bundle. A host that has them is
unaffected.

## 4. What is checked before replacing

An upgrade is a sequence of checks; any failure stops it with the installed
binary untouched:

1. The running binary's file can be located.
2. The release has one asset for this platform and a checksum list naming it.
3. Everything is fetched over HTTPS; a redirect to a non-HTTPS URL aborts.
4. The download is bounded in size and time.
5. The asset's SHA-256 matches the release's checksum.
6. The archive contains the binary for this platform.
7. The extracted binary runs and reports the release version.
8. The target location accepts a new file.

Only then is the file replaced. Replacement is atomic: a failure, kill, or power
loss leaves the old binary or the new one, never a partial file. Temporary files
live next to the target and are removed on every exit path, including an
interrupt.

## 5. What is replaced, and what is not

- The file that was executed. A symbolic link on `PATH` stays in place and the
  file it points to is replaced.
- Nothing else: settings, sessions, and the working directory are untouched, and
  no lock file is written.
- A package-manager install is still replaced when the location is writable; sal
  does not detect package managers. A non-writable directory is reported without
  trying to elevate privileges.
- A binary installed by `go install` is replaced, but the module cache is not;
  use `go install ...@latest` for those installations.

On Windows the running file is moved aside, the new file takes its place, and the
next start removes the leftover best effort.

## 6. Failure behaviour

Each failure prints one `sal: ...` line and exits 1; the installed binary keeps
working. Messages name the situation:

| Situation | Message names |
|---|---|
| No network, DNS failure, TLS failure | the release could not be reached, with the underlying error |
| GitHub rate limit (403 or 429) | the rate limit and a suggested wait |
| Repository or release missing (404) | that no release was found |
| No asset for this platform | the platform |
| Checksum file missing, or without this asset | which of the two |
| Checksum mismatch | failed verification; nothing was replaced |
| Archive missing the binary | what was found instead |
| Extracted binary does not run or reports the wrong version | the download was rejected; nothing was replaced |
| Target directory not writable | the directory; the binary was left unchanged |
| Target read-only or on a `noexec` filesystem | the target path |
| Interrupted (Ctrl-C) | nothing further; temporary files are removed |

A failed upgrade never leaves a partially written binary and never deletes the
installed one. Running the command again is safe.

## 7. What the upgrade does not do

- Downgrade, or install a pinned or prerelease version.
- Verify a signature. The checksum list and the asset come from the same release,
  so a compromised publisher can serve both.
- Update anything besides the binary: no session migration, configuration edits,
  or startup step.
- Use network mirrors, custom proxies, or GitHub credentials; only the standard
  system proxy environment is honored.
- Run as a tool or from the web console.
- Run as a background or scheduled check; `--check` is for the user or a script.
