# Self-upgrade

This document is the technical description of the feature specified in
`design/features/self-upgrade`: what the program is made of to upgrade itself,
which part owns which decision, what flows between the parts, and the invariants
the structure keeps. What the commands print and do is the feature document;
the shape of the code inside a part is the code's own business.

## 1. Shape

Two things are added, and neither is part of the agent:

- **A self-upgrade part**, a package of its own under `internal/`, responsible
  for everything an upgrade needs: asking GitHub for the latest release,
  choosing the asset for this machine, fetching and verifying it, unpacking the
  binary, checking that it runs, and handing a verified file to the platform
  replacement. It does not know about prompts, sessions, the terminal UI or the
  web console, and nothing in the agent knows it exists.
- **A command in the entry point**, which recognises the reserved first word,
  parses the subcommand's own flags, builds the upgrade part with the real
  filesystem, HTTP client and output stream, and reports the result. The entry
  point stays the only place that wires parts together.

The dependency direction stays a straight line: the entry point depends on the
upgrade part, and the upgrade part depends on nothing else in the module. That
is deliberate — an upgrade is meaningful even when the rest of the program is
broken, and it must be testable without a provider, a session or a terminal.

Three responsibilities are separated inside the part:

- **Release discovery** owns the conversation with GitHub: which release is the
  latest, and what it contains. It answers with a description of the release,
  not with a binary.
- **Asset handling** owns everything that turns a release description into a
  verified file on the local filesystem: platform mapping, name matching,
  download limits, HTTPS enforcement, checksum verification, archive extraction,
  the test run, and temporary-file cleanup.
- **Platform replacement** owns the one operation that differs between operating
  systems: putting a verified file where the running binary lives, atomically,
  while the process that is doing it is still running.

The split exists because these three have different reasons to change — GitHub's
API, the project's release naming, and the operating system's rules — and
because only the last one needs per-platform code.

## 2. Where the version comes from

The upgrade compares an installed version against a release version, so the
installed version has to be trustworthy in every way sal is built:

- a release build carries the tag it was built from, injected at link time by
  the release pipeline;
- a module install carries the module version the toolchain records in the
  build information;
- every other build reports an explicit development marker.

The order is: injected version, then build information, then the development
marker. This is an invariant: a release build must never report the marker, and
a development build must never claim a release version. A version that is not
semantic is never ordered against a release; the feature document says what the
user sees in that case.

Version comparison is a small part of its own inside the upgrade package, with
no dependency, because it is the only piece of parsing on the critical path and
the only one whose result decides whether anything is downloaded at all.

## 3. Release discovery

One request to the GitHub release API, for the latest non-draft,
non-prerelease release of the project repository. The answer is reduced to the
four things the rest of the upgrade needs: the tag, the release page URL, the
asset names with their download URLs and sizes, and whether a checksum document
is among them. Everything else GitHub returns is ignored, and no second API
request is made: asset downloads are plain HTTPS file fetches, not API calls.

The repository and both HTTP base URLs are configuration of the part, not
constants buried in the request code, so tests can point them at a local server
without a network. The user-facing program always uses the real ones.

Failures are classified where they happen, because the kind of failure decides
the message and whether retrying is worth suggesting: unreachable network, rate
limit, missing repository or release, and a malformed answer are distinct
outcomes, and none of them is panicked on or mistaken for "no update".

## 4. Asset selection and download

Selection is a pure function of the machine's operating system and
architecture, the release's asset names, and the naming rule the release
pipeline uses. Two properties matter:

- **The mapping is total or explicit.** Known platforms map to one archive form
  (two names, one for each packaging format); an unknown platform produces a
  named error instead of a guess.
- **Android is not folded into Linux.** The two builds differ in where the
  program looks for the system's DNS configuration, so a Linux binary on an
  Android device starts but cannot resolve a name — a failure the test run
  cannot see, because reporting the version needs no network. The platform
  table therefore keeps Android separate and the release builds it separately.
  Only Android `arm64` is published: an Android build needs cgo, and the
  release pipeline has no cross toolchain for the other architectures.
  A mapping that looks equivalent because the machine runs the other platform's
  binaries is exactly the kind of guess this rule exists to prevent.
- **Matching is by suffix, not by an exact assembled name**, so a release that
  changes the arrangement of the name around the platform does not break
  installed copies. It must still match exactly one asset and a checksum entry;
  zero or several are errors.

Downloading and verification are one step with a fixed order and fixed limits:

- HTTPS only, checked before the request and again after any redirect;
- a bounded body and a bounded total time, so a stalled or endless response
  cannot fill the disk or hang the command;
- the file is written into the same directory as the target, so the final
  replacement is a rename within one filesystem and never a cross-device copy;
- the checksum is verified before anything is unpacked, against the release's
  own checksum document, and a missing or ambiguous entry is an error rather
  than a skip;
- the archive is unpacked in memory or streamed, and the member is selected by
  the platform's binary name, not by position.

The test run before replacement is what makes "verified download" mean something
stronger than "the checksum matched": it executes the extracted file with the
plain version flag and requires the expected version. It is also the check that
notices a target filesystem that does not permit execution. Because it runs
before the replacement, its failure costs nothing but a temporary file — and
because it runs a binary just fetched from the network, it is the one deliberate
execution in the feature, documented rather than hidden.

All temporary state is created next to the target and removed on every exit
path, including a cancellation. Cleanup is unconditional; it never masks the
error that caused it.

## 5. Platform replacement

The part exposes one operation: given a verified file, put it where the running
binary lives. Everything before that point is platform-independent.

- **Locating the target.** The running executable's own path, with symbolic
  links resolved, so a link on `PATH` is preserved and an upgrade does not
  change the name the binary is invoked by.
- **Unix-like systems** replace by renaming the verified file over the target:
  the running process keeps the old inode, the new file appears atomically, and
  no window exists in which the target is missing or truncated.
- **Windows** cannot replace a running executable, so the running file is moved
  aside and the new file is renamed into its place. The moved-aside file cannot
  be deleted while the old process runs, so it is left behind and removed by
  the next start of the program, best effort and silently. The replacement
  retries briefly, because a virus scanner or an indexer can hold the file for a
  moment, and a rename that succeeds is still atomic from the user's point of
  view.
- **Failure is inert.** Any error from the replacement — permissions, a
  read-only mount, a filesystem without execute permission — leaves the
  installed binary in place and removes the verified copy. There is no partial
  state to recover from, which is why no rollback step exists.

The platform files are selected by build tag, and the shared part never calls a
platform-specific operation by a condition on the operating system; a build for
an unsupported platform either fails to compile the replacement in, or answers
with the "no asset" error before reaching it.

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
        │        verified file in the target directory
        │              │
        │              ▼
        │        test run (version flag)
        │              │
        │              ▼
        │        platform replacement ──▶ target file
        │              │
        ▼              ▼
     report (stdout/stderr, exit status)
```

`--check` stops after release discovery and version comparison; it never reaches
asset handling. That is a structural guarantee, not a policy check: the check
path has no access to the code that writes files.

The upgrade part reports through an injected output stream and returns errors
instead of exiting, so a test can read what the user would see and assert on the
returned error without spawning a process.

## 7. Invariants

- The installed binary is either untouched or fully replaced; a failure never
  leaves a partial or missing binary in place.
- Nothing outside the target file and the temporary files beside it is written;
  configuration, sessions and the web token are never opened.
- Every network request is HTTPS, and every byte that becomes a binary has a
  verified checksum from the release it came from.
- `--check` performs no filesystem writes and executes nothing it downloaded.
- The command needs no credential, no configuration file and no terminal.
- The upgrade never downgrades, never selects a prerelease, and never treats an
  unorderable installed version as either older or newer.
- The agent, the tools and both front ends are unaware of the feature: there is
  no tool, no slash command and no event for it.

## 8. Release pipeline

The feature is only usable if releases carry assets, so the pipeline is part of
the design:

- Tagging a release builds one static binary per supported platform and
  architecture, with the version injected, and packages each in the archive form
  the naming rule implies;
- it publishes the archives, the checksum document, and nothing else that the
  feature would depend on;
- it builds for every platform in the selection table, so the table and the
  pipeline cannot drift apart without a visible failure.

The platform list lives in one place in the upgrade part and is mirrored by the
pipeline's build matrix; a change to one without the other is a bug the feature
document's platform table would show.

## 9. Testing

- **Version comparison** as a table: ordering, prereleases, the `v` prefix,
  and the unorderable marker.
- **Release discovery and asset selection against a local HTTP server**: every
  platform in the table including the Android mapping, an unknown platform, a
  release missing the checksum document, and zero or duplicate asset matches.
- **Failure paths with the real filesystem in a temporary directory**: checksum
  mismatch, wrong archive member, a test run that reports the wrong version, a
  target directory that refuses the rename — each must leave the target's
  content unchanged.
- **Replacement on the host platform**: a file replaced in place, a symbolic
  link whose target is replaced and whose link survives, and a read-only
  directory reported as an error. These are written so they hold on every
  platform in the target set, not just the one running the suite.
- **The command itself** through the entry point against a local server, for
  both `--check` and the no-update path, asserting output and exit status.
- **Compilation of every supported platform** in CI, so the per-platform
  replacement file and the platform table are always buildable.
