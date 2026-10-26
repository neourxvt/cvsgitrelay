# CVS compatibility

The capability gate was exercised with GNU CVS 1.11.23 (client/server) on Linux,
including the `:fork:` remote protocol, and Git 2.56.0. Run it locally with
`go test ./internal/relay -run TestCVSCapability -v`. No public server is needed.
The captured, unmodified metadata grammar is in
`testdata/gnu-cvs-1.11.23.rlog`; only the disposable repository prefix is replaced.

The fixture demonstrates active revisions, removed files in Attic, dead trunk
revisions for files added on a named branch, magic branch symbols, vendor
revisions, static tags, absent commit IDs, multiline messages containing a
separator, and exact single-file `checkout -n -p -kb -r NUMBER` retrieval of NUL,
CRLF, non-UTF-8 bytes and keyword-looking content. Spaces in filenames work.

Supported module layouts are literal repository directories and directory
aliases composed of `-a` aliases and regular directory modules with optional
`-d` destinations. They must have a root directory at the requested module and
place all included directories beneath it. File selections, exclusions, hooks,
ampersand modules, overlapping source directories and unexplained server-side
path remapping are rejected.

`checkout -c` provides the definitions used to map physical RCS paths into Git
checkout paths. The definitions are resolved before and after discovery; the
layout is pinned in the Git checkpoint on the first import. Exact numeric
retrieval uses each file's physical repository path, since a composite alias
path can unexpectedly expand to several files even with `checkout -p`.

The public Schmorp module listing is captured in `testdata/schmorp-modules.txt`.
It defines `rxvt-unicode` as the main tree plus `libev` and `libecb`, mounted in
the corresponding subdirectories. A local remote-protocol fixture demonstrates
complete discovery, exact binary retrieval, independent trunk/vendor checkout
verification, restart/no-op behavior, and rejection of later alias remapping.
Only a terminal Attic component is removed. Newline, carriage return, tab and
NUL in paths cannot be represented unambiguously by this rlog dialect and are
rejected. Other POSIX path bytes are retained, with hex encoding in provenance.

Independent checkout fixtures use numeric major `-r 1` for trunk. GNU CVS `HEAD`
can follow a default vendor branch and is not the numeric trunk selector.
Named branch checkout fixtures include different per-file forkpoints, a file
with no branch changes, branch-only files, late additions, deletion/re-addition,
and vendor updates followed by a first ordinary trunk edit.

Each new client/server dialect needs its own capability run. A successful CVS
exit alone does not establish complete enumeration. The parser must validate
headers, declared counts, revision uniqueness and dependencies. A log message
that impersonates a complete record boundary must produce an error rather than
an inferred revision. No RCS archive is read directly by the exporter.
Discovery also rejects nonempty stderr after exit zero: a warning about an
inaccessible or skipped file cannot be treated as complete history. Remote
transports must keep login banners and unrelated messages off this stream.

The Go implementation replaces the proposal's C/Meson build with Go modules and
the standard library. Installed CVS and Git remain the only runtime dependencies.
