# cvsgitrelay

A Go command line mirror from CVS file revisions to a dedicated bare Git
repository. It uses the standard CVS client for discovery and exact byte
retrieval, and Git plumbing for objects and atomic ref publication.

Build with Go 1.24 or newer. Runtime dependencies are CVS and Git 2.48 or newer:

```sh
go build ./cmd/cvsgitrelay
cp example.toml mirror.toml
# Edit source and destination in mirror.toml first.
./cvsgitrelay ./mirror.toml
./cvsgitrelay status mirror.toml
./cvsgitrelay verify mirror.toml --mode metadata
./cvsgitrelay verify mirror.toml --mode deep
./cvsgitrelay verify mirror.toml --mode tip
```

The default operation takes a required TOML path directly; `status` and
`verify` take it after the subcommand. Optional overrides follow the path.
[example.toml](example.toml)
documents every setting, default, allowed value and CLI override. Unknown keys
and invalid types fail before repository access. Relative destination paths in
TOML are resolved against the file's directory; explicit `--repo` overrides are
relative to the caller. Other flags override TOML only when explicitly passed.
The default command initializes a new destination and imports its CVS history;
later runs import newly available history. `./sync-rxvt-unicode.sh` builds and
syncs using `example.toml`; run `./sync-rxvt-unicode.sh your.toml` to select a
different configuration.

For repeated debugging runs, create a local CVSROOT once and update it by
repeating the same command:

```sh
./cvsgitrelay mirror example.toml --output ./cvsroot-rxvt-unicode
./cvsgitrelay mirror example.toml --output ./cvsroot-rxvt-unicode
./cvsgitrelay example.toml --cvsroot "$PWD/cvsroot-rxvt-unicode" --repo ./git-debug-1
./cvsgitrelay example.toml --cvsroot "$PWD/cvsroot-rxvt-unicode" --repo ./git-debug-2
```

`mirror` uses the standard CVS client, including pserver authentication, and
requires no rsync or server filesystem access. `--output` is required and relative
to the caller; its parent must exist. The Git destination from TOML is unused.
Mirrors always include every supported revision line and included module,
regardless of `sync.scope` and `source.submodules`. `--connections` controls
concurrent revision downloads within each file. Existing generated CVSROOT copies
without a cache are adopted using their local revision contents.
Other nonempty CVS repositories are rejected.

Completed downloads are saved atomically in `.cvsgitrelay-mirror` inside the
CVSROOT. Repeated runs reuse cached revision bodies and annotations. Each run also
refreshes temporary current checkouts to observe permissions and empty directories.
An unchanged run performs zero historical revision downloads and zero archive
updates; the current-tree observation can transfer current file contents.
The summary reports `downloaded`, `reused` and `updated_files`. Interruptions keep
completed downloads for reuse. Failures, including a final metadata comparison
failure, retain the output directory, cached revisions, generated archives and
observation checkouts. Unpublished work is under `.cvsgitrelay-mirror/work-*`;
it remains available across failed retries and is removed only after a validated
mirror has been published successfully. Retry with the same `--output` directory
to reuse completed revision downloads. A writer lock prevents overlapping mirror runs.
Do not remove the internal cache if you want incremental updates and recovery.

For equal accessible source state and the same mirror format, fresh and
incremental mirrors converge to identical file paths, bytes, modes and epoch
mtimes, including administration and cache files. Output paths, CVS transports,
usernames, wall-clock time, polling schedule and connection counts do not enter
the generated contents. This guarantee covers the local CVSROOT; Git import
reproducibility is a separate concern described in
[the reproducibility proposal](doc-proposal-reproducibility.md).

Cached revision metadata and content digests are checked before reuse. Missing
or changed historical revision metadata and changed module layouts are rejected.
New revisions and updates to symbols, descriptions, keyword modes, default
branches, locks, access lists and observed permissions regenerate the affected
archives or their metadata. Existing revision payloads are
assumed immutable upstream; byte-only upstream edits cannot be detected without
refetching them. Corrupted local cached bytes fail their digest checks.

Archives are staged and checked with local CVS, including live revision bytes,
all revision annotations, diff statistics, locks and access metadata, before
publication. A second source scan rejects observed source changes;
it does not prove a repository-wide atomic source snapshot. Publication replaces
files atomically, one at a time. An interruption during publication can leave a
partially updated tree; rerun `mirror` to finish before importing from it. Run
readers after the mirror writer completes. Generated repository files are owned
by `mirror` and regenerated to match the source on subsequent runs.

Mirrors retain physical archive locations, empty directories, observed file and
checkout-directory permissions, module aliases, revision numbers, authors,
timestamps, states, logs, commit IDs, symbols, default branches, descriptions,
keyword modes, access lists, locks and strict-locking flags. Line provenance is
used to reconstruct deterministic deltas, retaining annotation results and
revision diff statistics. Custom states, branch aliases and non-1.x revision
numbers are accepted by mirroring independently of Git import restrictions.

Readable versioned `CVSROOT` archives are included with their complete histories.
Their current checkout contents are also preserved under
`.cvsgitrelay-mirror/administration/CVSROOT/`. The active local administration
uses a fixed configuration and selected module definitions; source hooks and
server settings are stored as data without being activated. Observed permissions
and directory layout are retained in `.cvsgitrelay-mirror/tree.json`, including
an availability flag for modes that CVS cannot expose. Inaccessible versioned
administrative history causes an error instead of an incomplete success.

Live revision bodies use exact `-kb` checkout bytes. Dead bodies and line origins
are recovered with `rannotate -F`; raw annotations are cached as well. Annotation
always adds a final newline, so an unterminated dead final line is recovered from
an identical live line with the same origin when available. When that last bit
is not exposed, the mirror retains the observable annotation and uses a trailing
newline. No byte-for-byte copy of the original RCS encoding, unversioned server
files or otherwise inaccessible metadata is claimed. Local CVS must reproduce
all parsed metadata and annotations before the command reports success.

The supported CVS text dialect and module-layout restrictions still apply.
Use a new Git destination when switching an existing Git mirror to a local
CVSROOT, because the source locator is part of its immutable identity.

`source.submodules = "exclude"` is the default. Included CVS modules such as
rxvt-unicode's libev and libecb contribute neither files nor revision histories
to Git. Set it to `"include"` to import them into the same Git repository at
their CVS checkout paths. This creates ordinary tracked directories, not Git
submodule links. Normal subdirectories of the main CVS source remain included.
Summaries report the policy, excluded module paths, and excluded file/revision
counts. Discovery still enumerates the full alias to validate its layout.
All verification modes use the same selection, including checkout comparisons.
The policy is pinned with the first imported revision and cannot change after
publication; use a new destination for a different selection. Older mirrors
with imported history implicitly used `"include"` and require that setting.

Use a literal repository directory module or a supported directory alias.
Directory aliases may combine `-a` definitions with `-d` destinations under the
requested checkout root. The selected source/destination layout is pinned in
the Git checkpoint with the first imported revision; later mapping changes
fail visibly. File selections, exclusions, checkout hooks and ampersand modules
are unsupported. Configure authentication through CVS
facilities; passwords embedded in CVSROOT are rejected. User settings live in
the required TOML file. Immutable source identity also lives in
`mirror.git/cvsgitrelay.json`, mode 0600, pinned by a fingerprint in Git; this
internal file is not a replacement for TOML and should not be edited.
The dedicated destination must initially be new or an empty bare SHA-1 repository.
New destinations use Git's reftable ref backend; existing empty bare
destinations are converted during init. This backend is required so the import
ref and ownership checkpoint share one atomic publication boundary.
Remote CVS sources remain read-only.

One CVS file revision creates one Git import commit, including dead revisions
and revisions with identical content. Files use mode 100644 and `-kb` retrieval.
CVS authors are preserved as hex metadata and shown as escaped ASCII Git names.
CVS timestamps use UTC. No changeset grouping or historical Git tags are inferred.

Sync fetches revision contents through up to four concurrent CVS connections
by default. Set `sync.connections` in TOML or pass `--connections N` to change
the positive limit; use `1` for one connection. Fetches use a bounded read-ahead
window, so increasing the limit also increases memory used for file contents.
Git commits and checkpoints are published serially in dependency order;
concurrent fetch completion does not change commit order. Metadata discovery
and verification remain sequential. Failed or interrupted syncs cancel and
wait for outstanding fetches before returning.

Trunk imports target `refs/heads/cvs/trunk`. Full metadata discovery occurs on
every run. Numbering gaps already present in CVS's listed history produce a
warning; accessible revisions are linked in numeric order without inventing
missing versions. Set `source.require_consecutive_revisions = true` in TOML
or pass `--require-consecutive-revisions` to reject numbering gaps instead.
This policy applies to sync, status and verify and can change between runs.
Declared revision counts must match the parsed records in either mode.
Previously imported revisions disappearing or changing, missing branch
forkpoints, unsupported source syntax and unexpected managed ref movement
fail visibly.

The default scope is `all` within the configured module selection. Named CVS branches use
`refs/heads/cvs/branches/<hex-name>`, initialized by an explicitly synthetic
orphan seed containing each member file's forkpoint. Source revisions retain
one canonical identity on their own revision line; seeds do not count as source
revisions. Vendor lines are included. Unnamed revision lines use per-file refs
under `refs/cvsgitrelay/unmapped/<hex-path>/<numeric-line>`; these are revision
archives, not complete project checkouts. Summaries report this limitation.
`--scope trunk` deliberately imports/verifies only numeric `1.N` history and
reports all remaining identities. Static CVS tags and merge reconstruction are
outside scope. Multiple symbols owning the same revision line are rejected
until branch alias semantics have an explicit policy.

Files added later on a named branch enter through their dead trunk forkpoint
and real branch revisions. A newly applied symbol that would retroactively add
a live forkpoint to an existing branch is rejected before publication. Named
branch trees are checked against independent CVS checkouts by `verify --mode
tip`. Trunk always means numeric `1.N` history; it does not follow CVS's mutable
default vendor branch. Use `git clone --branch cvs/trunk ./mirror.git checkout`
to check out the trunk mirror.

`refs/cvsgitrelay/state` contains an append-only Git checkpoint manifest. An
`update-ref --stdin` transaction advances both the import branch and this
manifest, with expected old object IDs. This small extension to the proposal
lets ref removal and rewinds be detected without an independent database or a
non-atomic local manifest. Published Git objects and refs remain authoritative.
Keep the state ref when backing up or transporting the mirror. See
[implementation decisions](docs/DESIGN.md) for the publication policy.

Each import begins with exactly four canonical provenance lines and a blank
line: `cvsgitrelay-Version`, `cvsgitrelay-Source-ID`, `cvsgitrelay-Kind`, and
`cvsgitrelay-Data`. Data is base64 of canonical JSON defined by `Metadata` in
`internal/relay/provenance.go`. It identifies the path, revision, predecessor,
ref, state, blob, source author, timestamp, optional CVS commit ID, default
branch, and log encoding/digest. Source path/author bytes use hex. The message
body retains the CVS-exposed log bytes, or reversible base64 if those bytes are
not UTF-8 or contain NUL. Source log text cannot override provenance.
The recorded default branch is an observation at import time; ordinary CVS
trunk check-ins may subsequently clear this setting without altering old
revision identities or their destinations.

Diagnostics go to stderr; stdout contains a single JSON operation summary.
Use `--log-format json` for collecting runtime reports and `--log-level debug`
for per-command timings and per-revision publication details. Normal operation
uses inventory, periodic progress, and summary counts. Failures are emitted once,
with a machine category, nested causes, stack frames, bounded subprocess stderr,
exit status, command duration, byte counts and revision/ref/object context.
Every report has a run ID. `--error-stacks=false` suppresses stack frames.
Text logs are human-readable; severity colors are used only on an interactive
terminal and are disabled by `NO_COLOR`. Ctrl-C gives active child processes
time to stop, kills any remaining helpers, cleans temporary work, and exits
without an error stack. Text failures use indented sections for context, causes,
and stack frames. JSON retains nested fields for machine collection.
Raw file contents, raw metadata scans, environment variables, and CVS locators
are not logged. Review diagnostics before sharing them: paths and usernames can
still be inferred from provenance and explicitly encoded source identities.

`--command-timeout` bounds each subprocess (default 5m). `--max-output` bounds
each command's retained stdout (default 256 MiB); exceeding it fails the run.
Both streams are drained concurrently. SIGINT/SIGTERM cancel command groups.
A local advisory lock excludes overlapping exporter processes, and Git ref
transactions also check for external ref changes. Keep exporter refs read-only
to other tools. Unreferenced objects after interrupted runs are harmless.

Exit codes: 2 arguments; 3 configuration; 4 unsupported source; 5 parse/source
integrity; 6 ref conflict or writer contention; 7 subprocess/deadline/output
limit; 8 intentional fault injection; 70 internal error; 74 local I/O.
Only successful operations exit zero.

See [CVS compatibility](docs/CVS_COMPATIBILITY.md) and [the proposal](PROPOSAL.md).
See [recovery and verification](docs/RECOVERY.md) for interruption handling.
The capability and integration fixtures use disposable local repositories and
need installed `cvs` and `git`; run them explicitly with `go test ./...`.
