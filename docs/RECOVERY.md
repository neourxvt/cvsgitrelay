# Recovery and verification

The default invocation takes the TOML path directly, as in
`cvsgitrelay FILE.toml`; `status` and `verify` take it after the subcommand.
Keep the file with the mirror's operational configuration. Runtime logging,
limits and verification settings can change. CVSROOT and module must match the
initialized source identity.
The included-module policy becomes immutable with the first imported revision.
Use a fresh destination to switch between `"exclude"` and `"include"`; editing
TOML cannot remove already published dependency history. Mirrors imported before
this setting existed require `source.submodules = "include"`. Mirrors initialized
without any imported revisions may use either policy. See `example.toml` for
all settings and overrides.

`cvsgitrelay verify FILE.toml --mode metadata` compares exact published
identities and immutable metadata with a new full CVS scan. It also checks each
revision's Git tree, controlled commit headers, ancestry, and the ownership
checkpoint chain.
`--mode deep` additionally retrieves every live revision again and checks its
recorded blob hash, then runs Git object integrity checks. `--mode tip` compares
an independent CVS checkout with the Git tree and requires a stable metadata
rescan. Supply `--scope trunk` deliberately for trunk-only verification.
Each mode checks only the configured module selection. Intentionally excluded
CVS modules appear as excluded counts, not missing revision identities.

The state ref and the affected data ref advance in the same Git transaction.
Every checkpoint is append-only and names exact expected data tips. A crash
before publication leaves unreachable objects; a crash after publication leaves
one recoverable revision identity. Rerun the same command. Do not remove
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
`before-publish`, `after-publish`, `seed-written`, and `seed-published`.
Without `kill:` the command returns an
intentional failure (exit 8). With `kill:` it terminates itself with SIGKILL.
The ordinal defaults to one. Never configure these in scheduled production runs.
The automated crash fixture kills the real CLI at each boundary, reruns sync,
and deep verifies the recovered archive, including interrupted branch seeds.

Temporary directories named `cvsgitrelay-tmp-*` may remain after SIGKILL. They
are disposable indexes and never authoritative state. Remove them only while
no exporter process is running. File locks are released by the kernel when a
process exits; the lock file itself can remain permanently.

For structured reports, retain stderr with `--log-format json` and the stdout JSON
summary. Retry with `--log-level debug` for bounded command and revision context.
Errors contain one full originating stack, wrapper locations, cause metadata,
and a run ID. File content and raw source scans are omitted.
The default text renderer prints separate, indented sections and one stack frame
at a time. JSON uses structured fields for collection by external tools.

The independent CVS-to-CVS command, `cvsgitrelay mirror FILE.toml --output DIR`,
keeps historical bodies and line provenance in the published RCS archives.
Compact compressed checkpoints in `.cvsgitrelay-mirror/archives/` record archive,
revision metadata, content and annotation hashes, plus final-newline hints.
Unchanged archives are checked and copied without replaying every revision.
Changed archives reuse verified historical contents from local CVS history;
only newly discovered revisions require downloads from the source.

Existing mirrors with full `revisions/` and `blobs/` caches migrate during the
next normal mirror run. Each archive is rebuilt and verified from those retained
downloads before they are reclaimed. This first run can take longer than later
polls, but does not download already cached revision bodies or annotations.
Pending downloads are compressed and retained until a verified staging archive
and its checkpoint are durable. Completed staging archives then replace those
downloads as recovery data, limiting expanded scratch data to the active file.

After an interrupted mirror run, rerun the same command and output directory.
Do not delete `.cvsgitrelay-mirror/pending/` or `work-*` directories: they may be
the only retained copy of downloaded history not yet published. Publication can
be partially complete; pending proofs recover from their checked staging
archives or matching published copies. Successful publication commits all
archive proofs before removing pending checkpoints and temporary work. A local
archive that no longer matches its checkpoint is rejected rather than silently
replacing historical downloads. Keep the checkpoints with the CVS repository.
