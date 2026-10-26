# Canonical Git format v2

This is the implemented SHA-1 reference format. Its executable contract is
`internal/canonical`, with complete object payloads and OIDs in
`internal/canonical/testdata/objects-v2.golden`. A format change requires a new
format ID unless all serialized reachable bytes remain compatible. No existing
v1 mirror is upgraded by installing or running this implementation.

## Source profile and supported limits

The reader profile is `gnu-cvs-rlog-v2`: ordinary GNU CVS client/server reads,
using complete unfiltered `rlog` output and exact numeric `checkout -n -p -kb`
retrieval. Remote `:pserver:`, `:ext:`, `:gserver:` and `:kserver:` locators and
local absolute, `:local:` and `:fork:` locators share this one profile. The chosen
transport must be supported by the installed CVS client. No server filesystem
access, administrative privileges or preliminary local archive copy is required.

Archive identities are repository-relative physical paths, without `,v` and
without an immediately enclosing `Attic` component. Duplicate normalized
archives, ambiguous paths and file/tree collisions are rejected. Module remapping
happens after archive identity is established. Directory aliases and `-d` mappings
are resolved using the existing literal module grammar. All mappings, including
excluded module mappings, are pinned in canonical policy. `submodules=exclude`
omits nonroot mappings and their shadowed checkout directories. Changing that
selection requires a new destination.

Source IDs contain 1..256 ASCII bytes in the range 33..126, inclusive. They are
supplied explicitly, not generated. Archive and checkout paths are byte strings,
at most 4096 bytes. Absolute paths, empty components, `.`, `..`, case-insensitive
`.git`, literal `CVS`, NUL, CR, LF and tab are unsupported. Module paths must also
be literal UTF-8 paths supported by the module resolver. Ref names are at most
4096 bytes; raw symbols are at most 1024 bytes. Reftable storage avoids filesystem
component limits for hex-encoded internal line IDs. New destinations use bare
SHA-1/reftable repositories; an existing empty bare repository is converted
during initialization. Different object formats and later ref-backend changes
are rejected.

Numeric components are canonical decimal unsigned 64-bit integers. Actual
revision numbers have positive components and an even component count of at least
two. Trunk revisions may use any positive major number. Magic `.0.` branch symbols
normalize to the corresponding odd-component numeric branch. Comparisons of
revision numbers use numeric tuples, never decimal text sorting.

Ancestry is the logical GNU CVS revision tree exposed by a complete listing.
Validate each file's reported total/selected counts and head. Sort all reported
revisions on each numeric line, linking each to the previous reported revision.
The first revision on a nontrunk line links to that line's numeric forkpoint,
which must exist. Every populated branch must also appear exactly once in its
forkpoint revision's `branches:` metadata; declarations without matching lines,
wrong forkpoints and duplicate declarations fail closed. A branch with no events
may exist through a symbol selecting a valid forkpoint. Gaps are accepted using
these complete listed sequences, never by subtracting a revision number.

The CVS interface does not expose raw RCS `next` storage links. This profile
validates the logical numbered CVS tree, not arbitrary malformed archive graphs
hidden behind a server. Server-hidden revisions remain outside observable scope;
matching scans do not prove an atomic repository snapshot. A previously accepted
revision's normalized parent remains immutable. Newly appearing records that
would change that parent fail source verification.

Supported states are exactly `Exp` (normalized to `live`) and `dead`. Source
seconds are in 0..253402300799, inclusive; GNU CVS log dates are UTC. Authors
contain 1..256 bytes. NUL in metadata output and unknown metadata fields or states
are rejected. The GNU CVS 1.11.23 dialect is exercised through a real CVS server
and captured output. Metadata header whitespace is parsed according to that
dialect; source path, author and log bytes are otherwise preserved.

Log bytes are the bytes between the supported revision header (including its
optional `branches:` line) and the next validated revision/file boundary. Split
only on LF, preserving CR and non-ASCII bytes. Rejoin every consumed line with
its LF; zero consumed lines encode an empty log. GNU CVS may add a terminating
LF to a nonempty original log, so the original archive's distinction between a
missing/present final LF is not recoverable. The canonical record preserves the
served log, not an invented original string. Separator-like lines remain log
bytes unless they form a complete structural boundary; count/framing failures
are rejected rather than repaired.

Live contents are exactly the served numeric `-kb` checkout bytes. A failed or
partial command aborts acceptance. For dead revisions, checkout output does not
reliably distinguish an inaccessible body from a present empty file, so this
profile consistently encodes content as `absent`, with no content read or bytes.
Their key, immutable metadata, ancestry and deletion event are still preserved
and verified. Live absent content is invalid. The pure builder can also encode
present dead content in normalized fixtures; this is not a second source reader.

Current default branches, keyword modes, descriptions, lock/access information
and commit IDs do not enter immutable records. Complete current symbols do enter
source equality. Numeric ancestry, rather than DefaultBranch, defines vendor
lines. CVS and Git subprocesses use controlled locale and timezone settings.

## Binary framing and fingerprints

`U(n)` is unsigned 64-bit big endian; `I(n)` is signed 64-bit big endian.
`B(x)` is `U(len(x)) || x`. Domain strings below include exactly one trailing
NUL before the payload. Integers have no floating-point representation.

```text
Key = B(source_id) || B(archive_path) || U(component_count) || U(component_1) ...
Ancestry = U(0)                         # absent parent
         | U(1) || Key(parent)          # one explicit parent
Record = Key || I(timestamp) || B(author) || B(log) || B(state)
         || B(Ancestry) || B(content_presence) || B(content)
Fingerprint = SHA256("cvsgitrelay-revision-record-v2\0" || Record)
```

State and content presence are ASCII `live`/`dead` and `present`/`absent`.
Absent content has a zero-length content field. A root's absent ancestry is a
framed eight-byte zero count, not an omitted ancestry field.

Policy bytes are the following length-framed strings, in this exact order:

```text
source_id
module
submodules                         # include or exclude
gnu-cvs-rlog-v2
attic-strip-v2
identical-membership-alias-v2
hex-author-v2
100644
kb
cvs-exp-dead-absent-v2
2
sha1
verification_policy                # full or trust-append-only
```

These are followed by `U(mapping_count)` and `B(physical_directory) ||
B(checkout_prefix)` for every mapping, sorted bytewise by physical directory.
Duplicate, overlapping physical mappings and duplicate destinations are rejected.
The empty checkout prefix denotes the root mapping. The policy digest is
`SHA256("cvsgitrelay-policy-v2\0" || policy_bytes)`.

The source digest is SHA-256 with domain `cvsgitrelay-source-v2\0`, over:

```text
U(revision_count)
    B(Key) || B(Record)              # sorted by raw encoded Key
U(symbol_count)
    B(Symbol)                       # sorted by (raw name, raw archive)
```

Each symbol belongs to one archive. Its bytes are `B(archive) || B(name) ||
B(kind) || B(target)`, with ASCII kind `branch` or `tag`. A branch target is
normalized numeric branch text. A tag target is the full encoded revision key.
One name cannot simultaneously identify branch and tag symbols in different
archives. Duplicate symbol records fail.

## Lines and replay

A file-line ID is `B(archive) || B(numeric_line_text)`. Trunk uses the text
`trunk` in place of a numeric line. Logical line IDs are:

- ASCII `trunk` for the common project trunk;
- `named\0 || primary_raw_name` for a named line;
- `numeric\0 || file_line_ID` for each unassigned numeric file line.

Names are aliases only when their complete file-line memberships are identical.
The primary is the bytewise-smallest alias. Partial overlaps fail. Symbols also
create branch members without events. All members of a nontrunk line must have
forkpoints on a common parent logical line. Unsupported parentless branch seeds
and dependency cycles fail. Parent lines are processed before children; ties use
logical line IDs. Distinct independent lines do not depend on one another's
processing order.

Each line starts with a synthetic empty root or a seed containing exactly its
members' forkpoint files. Dead forkpoints contribute no file. A seed's parent is
the member forkpoint event smallest by `(timestamp, checkout_path_bytes,
numeric_revision_tuple, archive_path_bytes)`. All actual events then replay using
Kahn's topological order with that same ready-event tie breaker. Each event
produces a commit even when its tree is unchanged.

Each tag selects exact per-file revisions, sorted by raw checkout path. Files
without that tag, and dead selected revisions, are absent. The selection digest
uses domain `cvsgitrelay-tag-selection-v2\0`, followed by `U(count)` and, for
every selection, `B(B(path) || B(Key))`. Its snapshot is a parentless synthetic
commit. Branch/tag movement and deletion are current-state inputs, not source
mutation of old immutable revision records.

## Git serialization

Object IDs hash `type + " " + decimal(payload_length) + NUL + payload` with
SHA-1. Blobs preserve their bytes. Trees use raw Git binary entries, fixed file
mode `100644`, directory mode `40000`, and Git sorting with a virtual trailing
slash for directories. No working tree, attributes, filters, signatures or local
text conversion participate.

Commit headers appear in this order: tree, ordered parents, author, committer,
one blank line, then the message. All timestamps have timezone `+0000`. Synthetic
commits use `CVSGITRELAY <relay@invalid>` for both identities and timestamp
946684800. Event authors use `CVS-<lowercase hex author bytes> <cvs@invalid>`;
their committer is `CVSGITRELAY <relay@invalid>`. Both event timestamps are the
source revision seconds. This profile has no configurable author map.

Messages contain precisely these ASCII records, with lowercase hex for arbitrary
bytes and one final LF. No extra blank lines or prose are permitted:

```text
cvsgitrelay-revision-v2
source=<hex source>
archive=<hex archive>
revision=<canonical decimal dotted number>
state=<live or dead>
timestamp=<decimal source seconds>
author=<hex author>
log=<hex full log bytes>
```

The root message is `cvsgitrelay-root-v2\nline=<hex logical line ID>\n`.
Seed messages contain `cvsgitrelay-seed-v2`, `line=<hex ID>`,
`forkpoint-anchor=<hex Key>`, then `member=<hex pair>` for each encoded
`B(file_line_ID) || B(forkpoint_Key)` pair in ascending byte order. Every field
occupies its own LF-terminated line. Tag messages have three lines:
`cvsgitrelay-tag-v2`, `name=<hex raw name>`, and `selection-sha256=<digest>`.
Anchor and state messages are exactly `cvsgitrelay-anchor-v2\n` and
`cvsgitrelay-state-v2\n`.

Line projections use `refs/cvsgitrelay/lines/b<hex logical ID>`. Trunk also uses
`refs/heads/cvs/trunk`; every named alias uses `refs/heads/cvs/b<hex raw name>`.
Tags use `refs/tags/cvs/b<hex raw name>` and point directly to their snapshot
commit. Hex encoding is reversible and collision-free.

## Manifest and reachability

The state tree contains exactly `manifest.v2`, mode 100644. Its ASCII grammar is:

```text
cvsgitrelay-manifest-v2
format=2
git-object-format=sha1
source=<hex source ID>
policy-sha256=<64 lowercase hex digits>
source-sha256=<64 lowercase hex digits>
verify=<full or trust-append-only>
revision-count=<decimal>
R <hex Key> <hex Fingerprint> <40-digit event OID>
line-count=<decimal>
L <hex logical line ID> <40-digit line tip OID>
ref-count=<decimal>
P <hex refname> <40-digit target OID>
```

R/L/P rows repeat exactly their preceding counts. They are sorted by their raw
encoded keys. There are no annotation lines, optional fields, duplicate keys,
extra spaces or blank lines. The file ends in one LF. Parsing requires an exact
round-trip through the serializer. Fingerprints suffice to compare complete
freshly read records after restart; the journal and cache are not required.

Gather every logical line tip and every tag snapshot, sort and deduplicate their
raw OIDs. Repeatedly group at most 64 contiguous OIDs into empty-tree synthetic
anchor commits with that parent order. A singleton passes through. Sort and
deduplicate the next level before grouping again. The state has that final anchor
as its sole parent, or no parent for an empty source. An empty source has no lines
or project roots. Previous state commits are never parents of a new state.

The builder validates every real commit/tree edge and returns only objects
reachable from the state. Scratch objects and dead-body blobs referenced only
by their immutable fingerprints need not survive GC. Every designated revision
event, tag snapshot, line tip and their trees/blobs must survive with the state
ref alone. Object storage verifies complete raw payloads against both the encoder
and Git; the built-in empty tree is explicitly stored when referenced.

## Publication, recovery and upgrades

Initialization reserves the dedicated repository and policy without creating a
state ref. Opening a destination acquires its exclusive exporter lock, validates policy and
object format, and completes recovery before accepting another source inventory.
Source acceptance verifies all previously accepted keys, then compares two full
normalized scans. An observed source change fails immediately; a later invocation
can retry. No partial scope publishes. Unchanged input returns without canonical
object or ref writes.

New objects are written and verified before a publication journal is rooted at
`refs/cvsgitrelay/ops/publish`. The journal is a Git commit whose parents protect
the new and old states. Its tree contains a JSON intent with version, prepared
phase, old/new states, expected/desired projection maps and a domain-separated
SHA-256 checksum. These operational bytes are excluded from canonical equality.

Before the first state switch, a fsynced local publication witness is installed.
An unpublished destination has no state or witness. A missing state with a
witness and no valid old=nil journal fails closed; it is never treated as a fresh
import. Recovery of a validated uncommitted old=nil intent removes its witness
after retiring that journal.

The state ref switches with a no-dereference expected-old CAS. The projector
then CAS-updates or deletes each projection, checking current state and all owned
refs at every step. Each mutation verifies its state and journal in the same Git
writer transaction, and journal retirement verifies the completed projection map. Only a recorded old or new value is acceptable. It verifies
the final projection map and retires the journal. These are individual projection
updates: [Git's update-ref semantics](https://git-scm.com/docs/git-update-ref) do
not give readers a multi-ref snapshot.

On restart, an old-state journal is discarded only if old projections still
agree. A new-state journal completes reconciliation idempotently. Unknown states,
unknown projection values, damaged/missing journal objects and mismatched
projections without a journal stop recovery. A lone state can retain its graph
through GC while projections are being reconciled. Operational refs under
`refs/cvsgitrelay/ops/` and explicitly retained backups under
`refs/cvsgitrelay/backup/` are excluded from canonical comparison; the active
publication journal is subject to its own strict validation.

Git loose-object/reference fsync is enabled. Tests exercise process crashes,
including SIGKILL, and kernel lock release. Power-loss durability remains
dependent on Git, filesystem and storage semantics and is not inferred from
those tests. Concurrent destructive GC and noncooperating writers are unsupported.
Lost recovery evidence is not repaired by guessing projection ownership.

Upgrade by building a new destination with the chosen source ID and policy, then:

```sh
cvsgitrelay coverage OLD.git NEW.git
```

This command checks that every v1 event identity exists in v2 by checkout path and
numeric revision, compares live blob bytes and deletion states, and reports
missing or changed identities. Both repositories must be fully reconciled; it
does not recover or alter either repository's refs or objects. The v1 reader is
restricted to this comparison. Existing imports cannot continue through v1 code,
and no in-place rewrite/migration command is provided. Consumer switching remains
an explicit operator action. The old mirror remains intact.

## Tests and performance

`go test ./...` runs versioned object/message goldens, generated small evolution
schedules, byte/path/integer limits, numeric gaps, alias and nested/vendor/dead
branch fixtures, source mutation/missing/race checks, cache corruption, real CVS
binary checkout comparisons, GC, locks, ref divergence, projection deletions,
state-consistent reads during projection interruption, v1 refusal/coverage, and
returned-error plus SIGKILL recovery boundaries. Source/destination path aliases,
processes, locale, timezone, inherited Git configuration and connection settings
are varied by fixtures.

Full verification reads O(total accepted source bytes). Reference replay stores
the full normalized inventory and reachable generated objects in memory, and its
simple sorted ready queue/tree construction can cost more than linear time. No
prefix-reuse optimization or changed-revisions-only cost is claimed. An unchanged
sync still verifies its source twice; it publishes no new objects or state.

Reproduce the small end-to-end benchmark, including verification and publication:

```sh
go test ./internal/relay -run '^$' -bench '^BenchmarkV2Sync$' -benchtime=1x -benchmem
go test ./internal/canonical -run '^$' -bench '^BenchmarkFullReplay$' -benchmem
```

Benchmark authors and initial revision timestamps are fixed independently of the
account and wall clock. Each fixture has four initial archives, nine revisions and binary/dead/vendor/
named-branch/tag coverage. Single-revision, older-archive and late-branch-member
cases start from a completed import. The benchmark reports source bytes and
reads, newly stored Git objects, elapsed time, allocations and revisions. Source
read counts include numeric live checkouts, rlog and CVS module-metadata
commands; byte counts include command stdout, excluding protocol framing. The
benchmark uses a local test server; its timings are not remote-throughput claims.

One illustrative single-iteration run on an Intel i7-13700K with Git 2.56.0
and GNU CVS 1.11.23 produced the following measurements. Fixture creation is
outside the operation timer. Temporary server path lengths affect rlog byte
counts; all source content, authors and timestamps are fixed by the fixture.
Object counts include operational journal objects.

| Case | Revisions | CVS requests | Response bytes | New objects | Time | Allocated bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Fresh | 9 | 20 | 5192 | 33 | 483 ms | 12541760 |
| No change | 9 | 20 | 5216 | 0 | 101 ms | 4334488 |
| One revision | 10 | 22 | 5556 | 12 | 510 ms | 12409464 |
| Earlier archive | 10 | 22 | 6082 | 25 | 507 ms | 14173480 |
| Branch member | 11 | 22 | 6522 | 16 | 504 ms | 13203264 |

Allocation totals are not peak resident memory. These small fixtures establish
request/object accounting, not production-scale CPU, memory or latency limits.
