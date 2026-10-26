# Proposal: reproducible CVS imports

Status: superseded design discussion. The implemented canonical importer is
documented in [the v2 format contract](doc-format-v2.md). This historical proposal
does not describe the current import algorithm or authorize migrating an
existing mirror.

## Required guarantee

Two machines importing the same logical CVS source state with the same import
policy and compatible exporter format must produce the same canonical Git
commits and ref targets. The result must not depend on destination paths,
machine identity, connection count, request completion order, polling schedule,
or interruptions followed by successful recovery.

In particular, importing a source state from scratch must agree with reaching
that state through incremental imports:

```text
fresh import of Sn
    ==
import S0, then S1, ..., then Sn
```

Here, `Sn` includes the accessible revision identities, exact file contents,
revision metadata, and the branch/module information used by the chosen model.
Equal file trees alone are insufficient: commit messages, headers, parents,
commit IDs, and canonical ref targets must agree.

Two separate properties need explicit treatment:

1. **Final convergence:** successful imports of the same source state produce
   the same final history, regardless of how they reached that state.
2. **Published hash stability:** a commit already published for a CVS revision
   keeps its ID as more history becomes accessible.

Rebuilding a canonical history can provide convergence while changing previously
published hashes. It must not be described as providing both properties.

The proposed comparison covers all exporter-owned refs, including
`refs/cvsgitrelay/state`, unless a different boundary is explicitly agreed.
Pack layout, compression, loose-object placement, reflogs, and unreachable
objects left by failed or superseded work are not required to be byte-identical.
They do not define the canonical reachable history.

## What currently changes the result

### Random source identity

[`Init`](internal/relay/repository.go) generates a random source ID for each new
destination. [`message`](internal/relay/provenance.go) embeds it in revision and
branch-seed commit messages. Consequently, independent fresh imports already
have different commit IDs before incremental ordering is considered.

The source identity needs a reproducible definition. Possible choices are an
explicit logical source identifier in configuration or a versioned digest of
canonical source settings. A random default would not meet the requirement.

Hashing the raw CVSROOT is insufficient for treating different transports,
usernames, or server aliases as the same logical source. Conversely, ignoring
the locator without another identity could conflate unrelated repositories.
The identity contract must say which differences are equivalent and how a
configuration mismatch is detected. Secrets must not enter public provenance.

The checkpoint's `ConfigHash` also hashes the current private configuration,
including CVSROOT. Making only `SourceID` deterministic would therefore leave
transport-dependent checkpoint hashes. Logical import identity and local
connection settings need separate treatment.

### Parents depend on the revisions available in each run

[`buildInventory`](internal/relay/model.go) sorts revisions by source timestamp,
destination ref, path bytes, and numeric revision. The scheduler then chooses
the first revision whose ancestry and branch seed dependencies are ready.

[`Sync`](internal/relay/import.go) applies this order to the currently pending
revisions and appends commits to published branch tips. It does not recompute a
single canonical branch history over all currently accessible revisions.

Consider two revisions on different files of the same Git branch:

```text
An incremental import initially discovers only A:
    root -> A

A later scan discovers B, which sorts before A:
    root -> A -> B          current append behavior

A fresh import sees both:
    root -> B -> A          canonical ordering of the complete set
```

Even if the final file trees match, the parent chains and commit IDs do not.
Changing A's parent to insert B also changes A's ID and descendant IDs.

Possible reasons for later discovery include:

- A repository-wide scan overlapping source changes. For example, it reads
  directory `a`, a writer commits B there and then A in directory `z`, and the
  scan later reads `z`. If locking permits this interleaving, the scan can see
  A without B. This is a scenario to demonstrate with a capability fixture,
  not an assertion that it has occurred in the configured public repository.
- Equal timestamps. The supported metadata records whole seconds. A revision
  created after a poll can share a timestamp with an imported revision while
  sorting earlier by the deterministic path/ref tie-breaker.
- Old archives or previously inaccessible revisions becoming visible. Such
  changes may instead be classified as unsupported source mutation; that
  classification must be explicit.

The conflict concerns insertions into the same materialized Git branch. A
revision appearing on an independent branch does not automatically change
another branch's parent chain, although it can change the global checkpoint
publication order.

### Import-time default-branch metadata

[`metadata`](internal/relay/provenance.go) copies the file's current
`DefaultBranch` into every revision's commit metadata. CVS can clear this field
after a normal trunk edit. Existing imported commits retain the old value;
[`validatePublished`](internal/relay/import.go) deliberately tolerates that
difference.

A fresh import after the field changes therefore writes different messages for
the same old revisions. Fixing source identity and ordering would not fix this.
Mutable observations need to stay outside immutable revision commits, or be
derived from an immutable historical source that is available to every import.
Normalizing the field only for future commits would leave existing hashes
different and needs an explicit format/migration policy.

### Branch seeds and symbolic branch assignments

[`prepareBranch`](internal/relay/branch.go) builds a seed from the branch members
visible when that branch is first imported. Its message also records membership.

If a file joins the branch later, an incremental import retains the original
seed and adds membership during later publication. A fresh import sees the
larger membership set when constructing its seed. Even a dead forkpoint that
adds no file content can change the seed's metadata, its ID, and descendant IDs.

Symbolic names also determine the destination ref recorded in revision
metadata. A newly named formerly unnamed line, moved symbol, or changed alias
policy can change fresh-import output while an existing mirror rejects the
change. Those rejections protect existing history but do not establish
convergence for the changed source state.

Branch identity, seed membership, naming, and late membership must have a
canonical rule independent of observation time. Current CVS symbols do not by
themselves provide a complete historical record of when membership was added.

### Checkpoints record publication history

[`publish`](internal/relay/repository.go) writes a manifest commit after each
data-ref publication, using the preceding checkpoint as its parent.
[`validateCheckpoints`](internal/relay/checkpoint.go) enforces that each step
advances one data ref and that its data commit extends the previous tip.

This is more precise than saying checkpoints are per polling run: an unchanged
run adds no checkpoint, and merely splitting an identical publication sequence
across runs need not change it. Different publication sequences, seed contents,
or intermediate scope selections can change the checkpoint chain.

Reproducible data-branch tips alone are therefore insufficient. Options include
a canonical replay of checkpoint transitions, a deterministic manifest commit
representing current logical state, or a separate operational audit history.
An operational journal cannot be included in a polling-independent equality
claim unless its own contents and ancestry are canonical too.

### Input bytes, serialization, and environment

Git commit identity includes the tree, ordered parent list, author and committer
headers, timestamps, and complete message bytes. Each needs a defined encoding
and format version.

The implementation already fixes file mode, byte retrieval with `-kb`, Git
identities and dates, UTC, and several Git configuration influences. It sorts
many path/ref collections and uses structured JSON metadata. These are useful
building blocks, not a complete reproducibility guarantee.

The design must also cover supported CVS output dialects, log-message newline
handling, arbitrary supported path/author bytes, symbolic-name encodings, module
mapping order, and any map traversal that affects publication. Changing a
serializer, parser, ordering rule, or provenance schema may intentionally change
IDs and therefore needs a compatibility contract. Logging, concurrency, local
paths, wall-clock time, and runtime limits must not enter canonical objects.

## The source-observation limit

The importer has no repository-wide snapshot identifier or global revision
sequence from the CVS interface it currently uses. Per-file counts and complete
parsing validate the returned records; they do not prove that records from
different files describe one atomic instant, or that an unseen older revision
will never become visible.

A second matching scan can reduce exposure to concurrent source changes, but
does not establish an immutable global ordering boundary. A fixed waiting period
also does not prove that later arrivals cannot sort earlier, without additional
source guarantees.

Reproducibility must be stated relative to equal accessible source inputs. No
importer can reproduce metadata or contents that one machine is never allowed
to observe. Previously imported history disappearing or changing must remain a
visible integrity failure, unless a separately agreed source-repair policy
defines otherwise.

## Design choices

### A. Canonical project history with affected suffixes rebuilt

Define a versioned canonical graph as a function of the complete accepted
source state and import policy. On each sync, compare the published graph with
that result, reuse matching objects, and rebuild the differing portions.

This can make a completed incremental import converge with a fresh import.
It requires non-fast-forward updates when a new revision sorts earlier or a
branch seed changes. Unchanged prefixes can retain their IDs; affected commits
cannot. Users following those refs need an explicit update contract.

The current append-only validators and publication model would need changes.
All affected refs and their canonical manifest must be published together with
expected-old-value checks. A crash must expose either a coherent old generation
or a coherent new generation, never a mixture. Content caches may accelerate
rebuilding but must not become authoritative state.

Reordering newly visible revisions must remain distinct from accepting edits
to previously imported source metadata or bytes. Reproducibility is not blanket
permission to accept upstream history corruption.

This option provides final convergence, not unconditional published hash
stability. No choice to implement it has been made.

### B. Stable revision archive with separate checkout histories

Give each CVS revision a canonical archival commit whose content and ancestry
depend only on immutable source information, rather than the current tip of a
project-wide import branch. Per-file ancestry is one possible basis, provided
it is stable and complete under the supported source contract.

Useful project checkout branches become derived views. Their aggregate commit
histories may need rebuilding when newly visible revisions change canonical
order, while the archival revision commits retain their identities. Derived
commits must not pretend to be second canonical imports of the same revision.

This separates stable source-revision identities from project-history ordering,
but changes the repository/ref model. It does not make aggregate branch commits
unconditionally stable. A newly revealed predecessor within a file also needs a
defined rejection or representation policy; per-file graphs alone do not solve
all late-discovery cases.

### C. Preserve append-only project history and reject conflicts

Before publishing, require the existing branch histories to be prefixes of the
canonical histories for the newly observed source state. Stop if an insertion,
changed seed, or other incompatibility would violate that condition.

Together with deterministic identity and metadata, this can prevent successful
runs from silently publishing divergent history. It cannot guarantee that every
valid source evolution can be imported successfully. It needs a documented
resolution path, and refusal alone must not be advertised as unconditional
incremental convergence.

### D. Obtain an authoritative source ordering or snapshot boundary

A durable global ordering supplied by the source, or a shared authoritative
journal available to both fresh and incremental imports, could establish which
prefix is complete before publication. Independent local discovery journals
would merely preserve their different observation orders.

This requires source capabilities beyond the current importer contract.
Timestamps or optional CVS commit IDs must not be assumed to provide such a
total ordering without demonstrating that property.

## Decisions needed before implementation

1. Does "across incremental runs" require final convergence, permanent hash
   stability, or both? Which refs carry each guarantee?
2. Are canonical project branches allowed to move non-fast-forward, or should
   stable revision archives be separated from derived checkout histories?
3. What identifies the logical source across transports and machines, and
   which configuration belongs to canonical identity rather than local access?
4. How are branch seeds, mutable symbols, and observation-time metadata
   represented without depending on the first import date?
5. Must the state ref be fully canonical, or can an explicitly separate
   operational history be excluded from comparison?
6. What source changes cause rejection, and what evidence is required before
   treating a returned inventory as suitable for publication?
7. How are existing mirrors with random source IDs and v1 provenance handled?
   A format change cannot preserve their old commit IDs while also matching a
   newly defined canonical format. Rebuild/migration must be explicit, must
   preserve the original until authorized, and must not silently edit identity
   files or reset published refs.

With a single linear project history, arbitrary earlier insertions, final
convergence, and permanent stability of published hashes cannot all be promised
together. Choosing the representation or the permitted source constraints is a
necessary part of meeting the requirement.

## Proposed acceptance checks

These are proposed checks for the eventual design, not changes implemented by
this document:

- Compare independent fresh imports in different destination directories and
  processes, with connection limits of 1 and 4, by canonical revision-to-commit
  mappings, complete commit bytes, ref targets, and the agreed state refs.
- Compare one fresh import against several incremental schedules ending at the
  same source state, including identical timestamps, reversed discovery order,
  and a source change during enumeration. Demonstrate any claimed CVS locking
  assumptions with a controlled fixture.
- Cover branch creation, dead and live late forkpoints, unnamed-to-named lines,
  vendor default-branch clearing, and different intermediate scope selections.
- Cover accepted initial numbering gaps and later changes to accessible
  ancestry, with explicit success or rejection expectations.
- Interrupt around fetch, object creation, and publication; after recovery,
  compare with an uninterrupted fresh import. Repeat with out-of-order fetch
  completion and a failed fetch.
- Confirm that unchanged source is a no-op, changed imported source is rejected,
  and machine-specific Git settings cannot alter canonical objects.
- Define expected behavior for existing v1 mirrors and for each supported
  exporter format transition.

Testing must distinguish a reproducible final graph from preservation of
previously published hashes. Passing one comparison does not establish the
other property.
