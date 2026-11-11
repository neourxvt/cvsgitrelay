#!/usr/bin/env python3
"""Development shim for cvsgitrelay.

Wraps the Go toolchain so the usual tasks are one short command:

    ./dev.py build                 build ./cvsgitrelay in the checkout
    ./dev.py test                  run all tests
    ./dev.py test relay --run Growing --race
    ./dev.py test -- -timeout=5m   pass further flags to go test after --
    ./dev.py fmt [--check]         gofmt the tracked Go files
    ./dev.py vet                   go vet ./...
    ./dev.py lint                  gofmt check, go vet and staticcheck
    ./dev.py check                 gofmt check, go vet and all tests
    ./dev.py run status example.toml
    ./dev.py clean                 remove the built binary

Every command runs from the checkout, so the shim works from any directory.
Only ``run`` keeps the caller's directory, so relative TOML paths and
destinations given to the binary resolve as they would in a direct call.
"""

import argparse
import os
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
BINARY = ROOT / "cvsgitrelay"
MAIN_PACKAGE = "./cmd/cvsgitrelay"


class Failed(Exception):
    """A step failed; carries the exit status the shim should return."""

    def __init__(self, status: int):
        super().__init__(status)
        self.status = status


def sh(*cmd: str, cwd: Path = ROOT, capture: bool = False, echo: str = "") -> str:
    """Run cmd from cwd, echoing it first, and raise Failed on a non-zero exit.

    With capture the standard output is returned instead of shown. echo
    replaces the echoed command line, for commands with long file lists.
    """
    print("+ " + (echo or shlex.join(cmd)), file=sys.stderr, flush=True)
    done = subprocess.run(cmd, cwd=cwd, text=True, stdout=subprocess.PIPE if capture else None)
    if done.returncode != 0:
        raise Failed(done.returncode)
    return done.stdout if capture else ""


def go_files() -> list[str]:
    """Return the tracked Go files.

    Untracked checkouts in the tree (CVS roots, Git destinations, other
    projects) are left alone this way.
    """
    out = subprocess.run(
        ["git", "ls-files", "-z", "--", "*.go"], cwd=ROOT, check=True, stdout=subprocess.PIPE, text=True
    ).stdout
    return [name for name in out.split("\0") if name]


def package(name: str) -> str:
    """Expand a short package name such as ``relay`` to ``./internal/relay``.

    Anything that already looks like a Go package pattern is passed unchanged.
    """
    if name.startswith((".", "/")) or "/" in name:
        return name
    for parent in ("internal", "cmd"):
        if (ROOT / parent / name).is_dir():
            return f"./{parent}/{name}"
    return name


def cmd_build(args: argparse.Namespace) -> None:
    sh("go", "build", "-o", str(BINARY), MAIN_PACKAGE)


def cmd_test(args: argparse.Namespace) -> None:
    cmd = ["go", "test"]
    if args.race:
        cmd.append("-race")
    if args.verbose:
        cmd.append("-v")
    if args.run:
        cmd += ["-run", args.run]
    if args.fresh:
        # A count of 1 is the documented way to bypass the test cache.
        cmd.append("-count=1")
    cmd += args.extra
    cmd += [package(name) for name in args.packages] or ["./..."]
    sh(*cmd)


def fmt_check() -> None:
    """Fail when any tracked Go file is not gofmt clean."""
    unformatted = sh("gofmt", "-l", *go_files(), capture=True, echo="gofmt -l <tracked Go files>").split()
    if unformatted:
        print("not gofmt clean:\n  " + "\n  ".join(unformatted), file=sys.stderr)
        raise Failed(1)


def cmd_fmt(args: argparse.Namespace) -> None:
    if args.check:
        fmt_check()
    else:
        sh("gofmt", "-w", *go_files(), echo="gofmt -w <tracked Go files>")


def cmd_vet(args: argparse.Namespace) -> None:
    sh("go", "vet", "./...")


def cmd_lint(args: argparse.Namespace) -> None:
    fmt_check()
    sh("go", "vet", "./...")
    if shutil.which("staticcheck") is None:
        print("staticcheck is not installed; skipping it", file=sys.stderr)
        return
    sh("staticcheck", "./...")


def cmd_check(args: argparse.Namespace) -> None:
    fmt_check()
    sh("go", "vet", "./...")
    sh("go", "test", "./...")


def cmd_run(args: argparse.Namespace) -> None:
    cmd_build(args)
    argv = [str(BINARY), *args.args]
    print("+ " + shlex.join(argv), file=sys.stderr, flush=True)
    # Replace the shim so signals and the exit status belong to the relay.
    os.execv(BINARY, argv)


def cmd_clean(args: argparse.Namespace) -> None:
    if BINARY.exists():
        print(f"+ rm {BINARY}", file=sys.stderr)
        BINARY.unlink()


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="dev.py", description=__doc__.split("\n\n")[0], epilog=__doc__.split("\n\n", 1)[1],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    sub = p.add_subparsers(dest="command", metavar="COMMAND", required=True)

    s = sub.add_parser("build", help="build ./cvsgitrelay")
    s.set_defaults(func=cmd_build)

    s = sub.add_parser("test", help="run go test (default ./...)", description="Run go test. Flags after -- go to go test unchanged.")
    s.add_argument("packages", nargs="*", help="packages; short names like relay mean ./internal/relay")
    s.add_argument("--run", metavar="REGEXP", help="only run matching tests")
    s.add_argument("--race", action="store_true", help="enable the race detector")
    s.add_argument("-v", "--verbose", action="store_true", help="verbose test output")
    s.add_argument("--fresh", action="store_true", help="bypass the test cache")
    s.set_defaults(func=cmd_test)

    s = sub.add_parser("fmt", help="gofmt the tracked Go files")
    s.add_argument("--check", action="store_true", help="only report unformatted files")
    s.set_defaults(func=cmd_fmt)

    s = sub.add_parser("vet", help="go vet ./...")
    s.set_defaults(func=cmd_vet)

    s = sub.add_parser("lint", help="gofmt check, go vet and staticcheck if installed")
    s.set_defaults(func=cmd_lint)

    s = sub.add_parser("check", help="gofmt check, go vet and all tests")
    s.set_defaults(func=cmd_check)

    # main hands everything after run to the binary; this entry only lists it.
    sub.add_parser("run", help="build, then run the binary with the remaining arguments")

    s = sub.add_parser("clean", help="remove the built binary")
    s.set_defaults(func=cmd_clean)
    return p


def main(argv: list[str]) -> int:
    # Split off go test passthrough flags by hand: argparse would otherwise
    # treat their values as package names.
    extra: list[str] = []
    if len(argv) > 0 and argv[0] == "test" and "--" in argv:
        at = argv.index("--")
        argv, extra = argv[:at], argv[at + 1 :]
    if len(argv) > 0 and argv[0] == "run":
        # Bypass argparse so flags such as --help reach cvsgitrelay itself.
        args = argparse.Namespace(func=cmd_run, args=argv[1:])
    else:
        args = parser().parse_args(argv)
    args.extra = extra
    try:
        args.func(args)
    except Failed as failure:
        return failure.status
    except KeyboardInterrupt:
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
