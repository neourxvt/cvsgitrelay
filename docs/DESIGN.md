# Implementation decisions

The implementation uses Go 1.24+ and the standard library. Linux/POSIX process
groups and advisory locking are explicit platform requirements. Git and CVS
remain subprocess backends; no CVS protocol, RCS delta, or Git object writer is
implemented inside the exporter.

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
