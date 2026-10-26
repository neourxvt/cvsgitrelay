# Implementation decisions

The implementation uses Go 1.24+ and a TOML parser library. Linux/POSIX process
groups and advisory locking are explicit platform requirements. Git and CVS
remain subprocess backends; no CVS protocol, RCS delta, or Git object writer is
implemented inside the exporter.

## Configuration and module selection

Every command requires a user-managed TOML file; `example.toml` documents the
complete settings schema. Explicit CLI flags override the file. Unknown keys,
type errors and invalid values fail before repository access. Internal source
identity remains in the private JSON file and is checked against TOML on open.

Included CVS modules default to exclusion. Resolve and validate the complete
module layout, then remove excluded files before constructing revision lines,
branch membership, seeds or objects. Known excluded checkout destinations are
also pruned from the main directory if their paths overlap. Ordinary nested
directories are retained. Tip verification expands the independent CVS checkout
and prunes those same destinations before comparing Git membership and bytes.

The checkpoint pins both the selected layout and inclusion policy in the first
source publication. Changing the policy after import requires a new destination;
silently switching it could reinterpret published identities or leave unwanted
dependency history reachable. Legacy imports without a recorded policy retain
their original inclusion semantics. A destination with no data refs can choose
either policy without rewriting its initial checkpoint.

## Publication and ref ownership

The proposal needs both Git-authoritative revision state and detection of
removed/rewound managed refs. A separate mutable local manifest introduces a
second checkpoint boundary. Instead, `refs/cvsgitrelay/state` records immutable
ownership manifests in Git, and each publication updates that ref together with
one data ref. Every prior manifest transition is validated on reopen.

The destination uses Git's reftable backend. Its update transaction publishes a
new table through one rename of the stack's `tables.list`, giving the data ref
and checkpoint one visibility boundary. This avoids relying on a sequence of
individual loose-ref renames. Empty existing destinations are converted during
init; nonempty destinations and later backend changes are refused.
See [Git's reftable update transaction specification](https://git-scm.com/docs/reftable#_update_transactions)
and [Git ref migration](https://git-scm.com/docs/git-refs).

Acceptance fixtures cover no-op/restart behavior, missing refs, source mutation,
concurrent exporter exclusion, and SIGKILL at import/seed boundaries followed by
deep verification. These are process-interruption guarantees. Power-loss and
filesystem durability need separate deployment validation. A hard-killed Git
process can leave its own lock files; never remove Git locks while a writer
could still be alive. Ref corruption or unexplained locks cause a visible
failure rather than an automatic forced repair.

## Branch checkout policy

Each file's symbolic branch mapping determines membership and forkpoints.
Named branch seeds are synthetic orphan roots. Later additions from absent
forkpoints are ordinary file-revision imports. Retroactive live membership and
multiple symbols owning one revision line are explicit unsupported states.
Unnamed branches retain per-file archives and carry no project-checkout claim.

The vendor fixture demonstrates that CVS `HEAD` can select the default vendor
line. Tip verification therefore uses numeric major `1` to independently
checkout trunk. The fixture also proves that a normal trunk check-in can clear
the default vendor setting; that mutable observation is not compared as old
revision metadata. Revision destinations and source authors/times/logs remain
immutable. The supported named branch tips, including vendor lines, are checked
against separate CVS working checkouts.

## CVS directory aliases

CVS module aliases can compose a project from directories elsewhere in the
repository. The configured module names the project checkout; `rlog` still
names each physical RCS archive. A resolved layout therefore maps repository
directories to checkout destinations, while each revision retains its physical
file path for exact retrieval. Taking only an RCS basename or fetching through
the composite checkout path would lose this distinction and can select the
wrong file or concatenate multiple files.

The supported subset is directory aliases (`-a`) and regular directory modules
with optional destinations (`-d`). Resolve the actual `checkout -c` listing,
reject cycles and ambiguous layouts, and require the same layout after the
metadata scan. Pin the selected layout in the same Git transaction as the first import.
Existing literal mirrors have an implicit literal mapping and remain readable;
their published histories cannot be reinterpreted as composite layouts.
The import commit schema and existing source configuration remain compatible.
