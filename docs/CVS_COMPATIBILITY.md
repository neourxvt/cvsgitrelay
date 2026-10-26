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

Compatibility is deliberately limited to literal repository directory modules.
RCS paths must match the repository directory from CVSROOT plus that literal
module. Aliases, module options and server-side path remapping are rejected.
Only a terminal Attic component is removed. Newline, carriage return, tab and
NUL in paths cannot be represented unambiguously by this rlog dialect and are
rejected. Other POSIX path bytes are retained, with hex encoding in provenance.

Each new client/server dialect needs its own capability run. A successful CVS
exit alone does not establish complete enumeration. The parser must validate
headers, declared counts, revision uniqueness and dependencies. A log message
that impersonates a complete record boundary must produce an error rather than
an inferred revision. No RCS archive is read directly by the exporter.

The Go implementation replaces the proposal's C/Meson build with Go modules and
the standard library. Installed CVS and Git remain the only runtime dependencies.
