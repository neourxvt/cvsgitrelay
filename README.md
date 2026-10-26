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
The exporter does not write to CVS.

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
