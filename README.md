# cvsgitrelay

A Go command line mirror from CVS file revisions to a dedicated bare Git
repository. It uses the standard CVS client for discovery and exact byte
retrieval, and Git plumbing for objects and atomic ref publication.

Build with Go 1.24 or newer:

```sh
go build ./cmd/cvsgitrelay
./cvsgitrelay init --cvsroot "$CVSROOT" --module project --repo ./mirror.git
./cvsgitrelay sync --repo ./mirror.git
./cvsgitrelay status --repo ./mirror.git
./cvsgitrelay verify --repo ./mirror.git --mode metadata
./cvsgitrelay verify --repo ./mirror.git --mode deep
./cvsgitrelay verify --repo ./mirror.git --mode tip
```

Use a literal repository directory module. Configure authentication through CVS
facilities; passwords embedded in CVSROOT are rejected. Source settings live in
`mirror.git/cvsgitrelay.json`, mode 0600, and are pinned by a fingerprint in Git.
The dedicated destination must initially be an empty bare SHA-1 repository.
The exporter does not write to CVS.

One CVS file revision creates one Git import commit, including dead revisions
and revisions with identical content. Files use mode 100644 and `-kb` retrieval.
CVS authors are preserved as hex metadata and shown as escaped ASCII Git names.
CVS timestamps use UTC. No changeset grouping or historical Git tags are inferred.

Trunk imports target `refs/heads/cvs/trunk`. Full metadata discovery occurs on
every run. Missing history, revision gaps, metadata changes, unsupported source
syntax and unexpected managed ref movement fail visibly.

The default scope is `all`. Named CVS branches use
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

`refs/cvsgitrelay/state` contains an append-only Git checkpoint manifest. An
`update-ref --stdin` transaction advances both the import branch and this
manifest, with expected old object IDs. This small extension to the proposal
lets ref removal and rewinds be detected without an independent database or a
non-atomic local manifest. Published Git objects and refs remain authoritative.
Keep the state ref when backing up or transporting the mirror.

Each import begins with exactly four canonical provenance lines and a blank
line: `cvsgitrelay-Version`, `cvsgitrelay-Source-ID`, `cvsgitrelay-Kind`, and
`cvsgitrelay-Data`. Data is base64 of canonical JSON defined by `Metadata` in
`internal/relay/provenance.go`. It identifies the path, revision, predecessor,
ref, state, blob, source author, timestamp, optional CVS commit ID, default
branch, and log encoding/digest. Source path/author bytes use hex. The message
body retains the CVS-exposed log bytes, or reversible base64 if those bytes are
not UTF-8 or contain NUL. Source log text cannot override provenance.

Diagnostics go to stderr; stdout contains a single JSON operation summary.
Use `--log-format json` for collecting runtime reports and `--log-level debug`
for per-command timings and per-revision publication details. Normal operation
uses inventory, periodic progress, and summary counts. Failures are emitted once,
with a machine category, nested causes, stack frames, bounded subprocess stderr,
exit status, command duration, byte counts and revision/ref/object context.
Every report has a run ID. `--error-stacks=false` suppresses stack frames.
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
limit; 70 internal error; 74 local I/O. Only successful operations exit zero.

See [CVS compatibility](docs/CVS_COMPATIBILITY.md) and [the proposal](PROPOSAL.md).
See [recovery and verification](docs/RECOVERY.md) for interruption handling.
The capability and integration fixtures use disposable local repositories and
need installed `cvs` and `git`; run them explicitly with `go test ./...`.
