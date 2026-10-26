# Recovery and verification

`verify --mode metadata` compares exact published identities and immutable
metadata with a new full CVS scan. It also checks each revision's Git tree,
controlled commit headers, ancestry, and the ownership checkpoint chain.
`--mode deep` additionally retrieves every live revision again and checks its
recorded blob hash, then runs Git object integrity checks. `--mode tip` compares
an independent CVS checkout with the Git tree and requires a stable metadata
rescan. Supply `--scope trunk` deliberately for trunk-only verification.

The state ref and the affected data ref advance in the same Git transaction.
Every checkpoint is append-only and names exact expected data tips. A crash
before publication leaves unreachable objects; a crash after publication leaves
one recoverable revision identity. Rerun the same sync command. Do not remove
the state ref, edit source settings, reset branches, or manufacture replacement
import commits. Restore damaged or removed refs from a complete backup.

An isolated ref deletion, rewind, unexpected advance, symbolic ref, metadata
edit or checkpoint edit is rejected. A coordinated rollback of the entire
repository, including all checkpoint history, cannot be detected without an
external audit anchor. Keep backups of all exporter refs. Git maintenance and
backup scheduling are external operations.

These guarantees cover process interruption. Sudden power loss also depends on
Git version, `core.fsync`, storage and filesystem behavior. This implementation
does not claim tested power-loss durability. Administrators needing it should
configure Git's `core.fsync=committed` policy and validate their storage stack.
Configuration is synced when initialized; no separate mutable cursor is used.

Explicit developer interruption points are available through
`--failpoint [kill:]STAGE[:ORDINAL]`. Stages are `scan-start`, `scan-complete`,
`fetch-start`, `fetch-complete`, `blob-written`, `tree-written`, `commit-written`,
`before-publish`, and `after-publish`. Without `kill:` the command returns an
intentional failure (exit 8). With `kill:` it terminates itself with SIGKILL.
The ordinal defaults to one. Never configure these in scheduled production runs.
The automated crash fixture kills the real CLI at each boundary, reruns sync,
and deep verifies the recovered trunk archive.

Temporary directories named `cvsgitrelay-tmp-*` may remain after SIGKILL. They
are disposable indexes and never authoritative state. Remove them only while
no exporter process is running. File locks are released by the kernel when a
process exits; the lock file itself can remain permanently.

For runtime reports, retain stderr with `--log-format json` and the stdout JSON
summary. Retry with `--log-level debug` for bounded command and revision context.
Errors contain one full originating stack, wrapper locations, cause metadata,
Go/build information, and a run ID. File content and raw source scans are omitted.
