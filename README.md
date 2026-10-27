# cvsgitrelay

Mirror every supported CVS file revision into a reproducible Git generation.

Build with Go 1.24 or newer, GNU CVS, and Git 2.48 or newer:

```sh
go build ./cmd/cvsgitrelay
cp example.toml mirror.toml
# Set source.source_id, the remote CVSROOT, the scope, and a NEW destination.
./cvsgitrelay mirror.toml
./cvsgitrelay status mirror.toml
./cvsgitrelay verify mirror.toml
```

[example.toml](example.toml) documents settings and CLI overrides. A TOML path
is required. Relative destinations belong to its directory. Explicit `--repo`
overrides are relative to the caller. An operator-assigned `source.source_id`
is mandatory; use the same ID for aliases of one logical source. CVSROOT,
credentials, destination paths, connection counts and local settings never enter
canonical objects. Policy changes require a new destination.

New destinations use SHA-1 objects and reftable storage. Empty bare destinations
are converted during initialization; existing populated mirrors are rejected.

Git import works directly with ordinary remote CVS access, including `:pserver:`
and `:ext:` CVSROOTs. It uses complete GNU CVS `rlog` listings and exact numeric
`checkout -p -kb` reads. Neither server filesystem access nor a preliminary
CVS-to-CVS mirror is required. Local and `:fork:` sources use the same CVS command
reader and normalization profile. For a local CVSROOT, sync and verify ignore
`sync.connections` and run one revision read per CPU core. Existing CVS authentication facilities remain
responsible for login and remote-shell credentials.

Every sync scans the complete configured scope twice, verifies every previously
accepted immutable record, then replays the full canonical graph. Equal final
source records, symbols and policy produce equal reachable object bytes and the
same state OID regardless of polling boundaries or discovery order. Newly visible
older revisions, branch membership changes, moved tags and deleted symbols may
rewrite project history. A matching second scan is a stability check, not proof
of a repository-wide atomic source snapshot. A local CVSROOT created by `mirror`
is scanned only once: sync and verify hold a shared lock on the mirror for the
whole read, `mirror` cannot update it meanwhile, and a sync started while
`mirror` runs fails with a lock error.

One designated event commit preserves each CVS revision, including dead and
unchanged-tree revisions. Trees use exact binary content and mode 100644. Named,
unnamed, vendor and nested lines are retained. Branch seeds and tag snapshots are
explicitly synthetic reconstructions; they do not represent atomic CVS changesets.
Partially overlapping branch aliases, inconsistent branch declarations, missing
forkpoints and unsafe paths fail closed. Ancestry follows each completely listed
numeric line and its declared forkpoint, including numbering gaps. Raw RCS delta
links and original log-string encoding are not exposed by remote CVS. Logs
preserve the supported `rlog` output bytes, including its newline normalization.

New destinations use format 3, where each event commit reads like a normal Git
commit. The CVS log message is the commit message; Git shows its first line as
the subject. One trailer line at the end names the CVS revision and archive,
which is all that verification needs besides the Git author and commit time:

```text
Author: root <root@cvs.invalid>

    Fix the build

    CVS: 1.225 rxvt-unicode/doc/rxvtd.1.man.in
```

Further trailer lines appear only when the commit header cannot carry the
information. `CVS-Author:` names the CVS username when the `[authors]` table
hides it, `CVS-State: dead` marks a removed file, and `CVS-Log:` restores the
exact log when it needs to be stored differently. A log without a final newline
is marked `CVS-Log: no-newline`. A log that is blank, contains NUL bytes or is
not valid UTF-8 is shown as readable substitute text, and its exact bytes are
kept as `CVS-Log: hex:...`. An archive path with unusual characters is written
as `hex:...` too.

The Git author is `<username> <<username>@cvs.invalid>`. The optional `[authors]`
table in the TOML file maps CVS usernames to real identities instead. Usernames
that are neither mapped nor limited to letters, digits and `._+-` keep the hex
form `CVS-<hex> <cvs@invalid>`. The author map is part of the canonical policy:
it is fixed when the destination is initialized, and a different map requires a
new destination. Synthetic commits (line roots, branch seeds, tags, anchors and
state) are unchanged.

Format 2 destinations, which use hex messages and `CVS-<hex>` authors, keep
working unchanged; they cannot be converted in place. To switch, sync into a new
`destination.repo`. Pointing `source.cvsroot` at a local `mirror` output makes
that conversion read only local files.

`verify.mode = "full"` is the default and is fixed in canonical policy. It detects
changed historical bytes, log messages, authors, timestamps, states and ancestry,
and missing revisions. Every unchanged run still re-fetches all live revision
contents. Dead revisions retain their immutable metadata and deletion event;
the CVS reader consistently represents their unavailable content as absent.
`"trust-append-only"` explicitly permits reuse of previously fingerprinted content
from a nonauthoritative local cache. It can miss old-content edits with unchanged
metadata. Damaged or missing caches fall back to source reads. Metadata is always
re-enumerated over the full scope; valid old cached content can skip a checkout.
Every report and manifest identifies the verification policy. `status` reads
only module definitions and `rlog` metadata: it reports discovered, imported
and pending revisions and rejects layout changes or disappeared revisions, but
never reads revision contents. Use `verify` for content checks.

`refs/cvsgitrelay/state` is the authoritative generation. Its deterministic state
commit contains `manifest.v2` and protects every canonical event and tag snapshot
through real parent edges. It has no parent edge to the previous generation.
An unchanged successful sync writes no canonical objects or refs. Initialization
reserves the repository and policy without publishing an unscanned inventory.
The first state is published only after a complete successful source scan. A
durable local publication witness distinguishes an unpublished destination from
a lost state ref.

Branches and tags are eventually reconciled projections:

- `refs/heads/cvs/trunk`
- `refs/heads/cvs/b<hex raw branch name>` for every named branch alias
- `refs/tags/cvs/b<hex raw tag name>` for exact per-file tag snapshots
- `refs/cvsgitrelay/lines/b<hex logical line ID>` for every line

The optional `[branches]` table selects and renames the branch projections.
Each entry maps `trunk` or a raw CVS branch name to a Git branch name, for
example `trunk = "main"` publishes the trunk as `refs/heads/main`. Only listed
branches get a `refs/heads/<name>` ref and no `refs/heads/cvs/` refs exist.
Several CVS names that denote the same branch may each be mapped to their own
Git name. `HEAD` follows the trunk, or the first Git name alphabetically if the
trunk is not listed. Tags and line refs are unaffected, and the history of an
unlisted branch stays in the object store, reachable from the state ref but
without a branch ref. The map is part of the canonical policy: a different map
requires a new destination. A listed branch that does not exist in CVS is
reported as a warning. A CVS branch literally named `trunk` makes the map
ambiguous and fails the sync.

Ordinary refs can expose mixed generations during publication. Consistent readers
resolve a single state OID and use every target from that manifest:

```sh
git fetch mirror +refs/cvsgitrelay/state:refs/remotes/cvs/state
generation=$(git rev-parse refs/remotes/cvs/state)
git show "$generation:manifest.v2"
```

Consumers tracking project branches must explicitly allow non-fast-forward fetches:

```sh
git fetch mirror '+refs/heads/cvs/*:refs/remotes/cvs/*' '+refs/tags/cvs/*:refs/tags/cvs/*'
```

Publication requires a reliable shared-filesystem writer lock. A separate Git
journal under `refs/cvsgitrelay/ops/publish` roots both generations before the state
CAS. Restart completes interrupted projections, accepting only their recorded old
or new values. Unexpected refs, missing recovery evidence and damaged objects stop
recovery without overwriting external changes. All cooperating publishers must
honor the lock. Independent remote writers and concurrent destructive pruning are
unsupported. Git object/reference fsync is enabled; process-crash tests do not
establish power-loss durability for arbitrary storage.

[The v2 format contract](doc-format-v2.md) specifies exact bytes, profile limits,
source normalization and executable goldens. Run `go test ./...` for the format,
source, convergence, GC, locking and recovery checks. Full replay retains the whole
normalized inventory and generated objects in memory; it has no changed-revisions-only
performance claim. Summaries report archive bytes/reads, newly written Git objects
and elapsed time. See the format document for reproducible benchmark commands.

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
concurrent revision downloads within each file, covering both annotations and
exact bodies, and concurrent historical permission probes. Existing generated CVSROOT copies
without a cache are adopted using their local revision contents.
Other nonempty CVS repositories are rejected.

Completed downloads are saved atomically in `.cvsgitrelay-mirror` inside the
CVSROOT. Repeated runs reuse cached revision bodies and annotations. Each run also
refreshes temporary current checkouts to observe permissions and empty directories.
Files absent from those checkouts, such as deleted files in `Attic`, need one
historical checkout each to expose their permissions. Those results are cached in
`.cvsgitrelay-mirror/probes.json` and reused while the probed revision and archive
location are unchanged, so an unchanged run makes no per-file requests. Pass
`--refresh-permissions` to repeat every probe, for example after permissions were
changed on the server without a new revision.
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
the generated contents. A server-side permission change without a new revision is
observed on the next run for files in the current checkouts; for other files it
needs `--refresh-permissions`. This guarantee covers the local CVSROOT; Git import uses the separate canonical graph described in
[the v2 format contract](doc-format-v2.md).

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
