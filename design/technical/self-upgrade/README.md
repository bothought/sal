# Self-upgrade

The technical design of `design/features/self-upgrade`: which parts an upgrade is
made of, what each owns, and the invariants that keep a failure inert.

## 1. Shape

Two pieces, neither part of the agent:

- **The upgrade part** (`internal/upgrade`) asks GitHub for the latest release,
  selects the asset, downloads and verifies it, unpacks the binary, checks that
  it runs, and hands a verified file to platform replacement. It knows nothing
  about prompts, sessions, or the web console.
- **A subcommand in the entry point** recognises the reserved word, parses the
  subcommand's flags, wires the part with the real filesystem, HTTP client and
  output stream, and reports the result.

The upgrade part depends on nothing else in the module, so it works when the rest
of the program does not and is testable without a provider, session or terminal.

Three responsibilities inside the part:

- **Release discovery** talks to GitHub and returns a description of the latest
  release, not a binary.
- **Asset handling** turns that description into a verified file: platform
  mapping, name matching, HTTPS, download limits, checksum verification,
  extraction, the test run, and temporary-file cleanup.
- **Platform replacement** puts the verified file where the running binary lives.

## 2. Version

The installed version, in order: the release tag injected at link time, the
module version from the build information, then the `dev` marker. A release
build must never report `dev`; a development build must never claim a release
version.

A version that is not semantic is never ordered against a release. `--check`
reports it as unorderable; an actual upgrade installs the latest release without
a comparison.

## 3. Release discovery

One request to the GitHub release API for the latest non-draft, non-prerelease
release. The answer is reduced to the tag, the release page URL, the asset names,
URLs and sizes, and whether a checksum document exists. Asset downloads are plain
HTTPS file fetches, not API calls.

Failures are named where they happen: unreachable network, rate limit, missing
release, or malformed answer. None is mistaken for “no update”.

## 4. Asset selection and download

Selection is a pure function of the machine, the release's asset names, and the
naming rule:

- every supported platform maps to exactly one archive form;
- Android maps to the Linux archive;
- an unknown platform is a named error, not a guess;
- matching is by suffix, so cosmetic changes around the platform in the asset
  name do not break installed copies. Zero or several matches are errors.

Download and verification have a fixed order and limits:

- HTTPS only, checked before the request and again after any redirect;
- bounded body and bounded total time;
- the file is written beside the target, so replacement is a rename within one
  filesystem;
- the SHA-256 is verified against the release's checksum document before
  anything is unpacked; a missing or ambiguous entry is an error;
- the archive member is selected by the platform's binary name, not position.

The test run executes the extracted file with the version flag and requires the
expected version. It runs before replacement, so a failure costs only a temporary
file, and it catches a target filesystem that does not permit execution. It is
the one deliberate execution of downloaded code.

Temporary state lives beside the target and is removed on every exit path,
including cancellation.

### 4.1 Android and Termux

The Go resolver reads `/etc/resolv.conf`, which Android does not write, and
Termux keeps its CA bundle under its own prefix. When the system files are
absent, the upgrade client reads the platform's resolver configuration and
certificate bundle. The fallback is confined to the client and never runs when
the system files exist.## 5. Platform replacement

One operation: place a verified file where the running binary lives.

- **Locating the target:** the running executable with symbolic links resolved,
  so a link on `PATH` survives and the invoked name does not change.
- **Unix:** rename the verified file over the target. The running process keeps
  the old inode; the target is never missing or truncated.
- **Windows:** rename the running file aside, rename the new file into place, and
  delete the old file on the next start, best effort. The move is retried briefly
  because a scanner or indexer can hold the file.
- **Failure is inert:** permissions, a read-only mount, or a `noexec` filesystem
  leaves the installed binary in place and removes the verified copy. There is
  no partial state and therefore no rollback.

Platform files are selected by build tag; the shared part never branches on the
operating system.

## 6. Data flow

```
argv "upgrade" [--check]
        │
        ▼
entry point ──▶ upgrade part ──▶ release discovery ──▶ HTTPS (GitHub API)
        │              │                                  │
        │              │◀──────── release description ─────┘
        │              ▼
        │        asset handling ──▶ HTTPS (asset + checksums)
        │              │
        │              ▼
        │        verified file beside the target
        │              │
        │              ▼
        │        test run (version flag)
        │              │
        │              ▼
        │        platform replacement ──▶ target file
        ▼
     report (stdout/stderr, exit status)
```

`--check` stops after discovery and comparison. It has no access to the code that
writes files. The part reports through an injected output stream and returns
errors instead of exiting, so tests can assert output and error without spawning
a process.

## 7. Invariants

- The installed binary is either untouched or fully replaced.
- Nothing outside the target and its temporary files is written; configuration,
  sessions and the web console are never opened.
- Every network request is HTTPS, including redirects, and every byte that
  becomes a binary has a checksum from its release.
- `--check` writes nothing and executes nothing downloaded.
- The command needs no credential, configuration file, or terminal.
- It never downgrades, never selects a prerelease, and never orders an
  unorderable installed version.
- The agent, tools and console are unaware of the feature: no tool, slash
  command, or event.

## 8. Release pipeline

Tagging a release builds one static binary per supported platform and
architecture, injects the version, packages each in the archive form the naming
rule implies, and publishes the archives and checksum document. The build matrix
covers every platform in the selection table, so the table and the pipeline
cannot drift apart silently.

## 9. Testing

- Version comparison as a table: ordering, prereleases, the `v` prefix, and the
  unorderable marker.
- Release discovery and asset selection against a local HTTP server: every
  platform, Android mapping, unknown platform, missing checksums, zero or
  duplicate matches.
- Failure paths with the real filesystem: checksum mismatch, wrong archive
  member, wrong reported version, non-writable target; each leaves the target
  unchanged.
- Replacement on the host: in-place replacement, a symlink whose target is
  replaced, and a read-only directory reported as an error.
- The command through the entry point for `--check` and the no-update path.
- Cross-compilation of every supported platform in CI.
