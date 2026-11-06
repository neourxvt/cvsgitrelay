"""Reusable pyinfra deploy for cvsgitrelay on Arch Linux.

Use it from another pyinfra deploy, for example::

    ext_module("cvsgitrelay").install()

``cvsgitrelay`` performs one sync per run, so it is installed as a oneshot
systemd service that a systemd timer starts periodically. A sync that fails
publishes nothing and the next timer run simply retries it.
"""

import hashlib
import subprocess
import tomllib
from functools import cache
from io import StringIO
from pathlib import Path
from textwrap import dedent

from pyinfra import host
from pyinfra.api import deploy
from pyinfra.facts.files import Sha256File
from pyinfra.operations import files, pacman, server, systemd


HERE = Path(__file__).resolve().parent
LOCAL_BINARY = HERE / "cvsgitrelay"
DEFAULT_CONFIG = HERE / "example.toml"

SERVICE_USER = "cvsgitrelay"
# The service owns this directory; the Git destination and the CVS login file
# of the service account live below it.
STATE_DIR = "/var/lib/cvsgitrelay"


def backup_locations() -> list[str]:
    """Return persistent data directories for hosts provisioned by ``install``.

    The Git destination can be recreated from CVS, but a fresh import reads
    the whole history again. Rewritten generations are also kept under
    ``refs/cvsgitrelay/superseded/`` and cannot be recovered from CVS.
    """
    return [STATE_DIR]


@cache
def build_binary() -> Path:
    """Build the executable once per deploy run, however many hosts use it."""
    # build.sh compiles into the checkout so the file is the one uploaded below.
    subprocess.run([str(HERE / "build.sh")], check=True)
    return LOCAL_BINARY


def pserver_login_entry(cvsroot: str) -> str | None:
    """Return the ~/.cvspass line for an anonymous pserver CVSROOT, if any.

    GNU CVS refuses a pserver without a stored login, even with an empty
    password. The scrambled form of the empty password is ``A``.
    """
    if not cvsroot.startswith(":pserver:"):
        return None
    # ":pserver:user@host:/path" and ":pserver:user@host:2401/path" are both valid.
    rest = cvsroot[len(":pserver:"):]
    user_host, _, path = rest.partition(":")
    if "@" not in user_host:
        return None
    user, _, hostname = user_host.rpartition("@")
    port = ""
    if path and path[0].isdigit():
        port, _, path = path.partition("/")
        path = "/" + path
    # CVS looks the root up in its canonical form, which always names the port.
    return f"/1 :pserver:{user}@{hostname}:{port or '2401'}{path} A\n"


@deploy("Install cvsgitrelay")
def install(
    config_file: str | Path | None = None,
    repo_dir: str = f"{STATE_DIR}/repo.git",
    install_dir: str = "/opt/cvsgitrelay",
    interval: str = "1h",
):
    """Install cvsgitrelay and sync CVS into a Git repository on a schedule.

    Args:
        config_file: Local TOML configuration; defaults to ``example.toml``.
            Its ``destination.repo`` is replaced by ``repo_dir``.
        repo_dir: Bare Git destination, which must be below the state directory.
            It is created by the first sync.
        install_dir: Directory containing the installed executable.
        interval: Time between the end of one sync and the start of the next.
    """
    source_config = Path(config_file) if config_file is not None else DEFAULT_CONFIG
    with source_config.open("rb") as handle:
        config = tomllib.load(handle)

    executable = f"{install_dir}/cvsgitrelay"
    config_path = "/etc/cvsgitrelay.toml"
    local_binary = build_binary()

    pacman.packages(
        name="Install cvsgitrelay runtime packages",
        packages=["cvs", "git"],
        present=True,
        _sudo=True,
    )

    server.group(
        name="Create cvsgitrelay group",
        group=SERVICE_USER,
        system=True,
        _sudo=True,
    )
    server.user(
        name="Create cvsgitrelay service account",
        user=SERVICE_USER,
        group=SERVICE_USER,
        home=STATE_DIR,
        shell="/usr/bin/nologin",
        system=True,
        create_home=True,
        _sudo=True,
    )
    files.directory(
        name="Create cvsgitrelay state directory",
        path=STATE_DIR,
        user=SERVICE_USER,
        group=SERVICE_USER,
        mode="0750",
        _sudo=True,
    )
    files.directory(
        name="Create cvsgitrelay install directory",
        path=install_dir,
        user="root",
        group="root",
        mode="0755",
        _sudo=True,
    )

    # Anonymous pserver sources need a stored login in the service account's
    # home, which the unit points HOME at.
    login = pserver_login_entry(config.get("source", {}).get("cvsroot", ""))
    if login is not None:
        files.put(
            name="Store the anonymous CVS pserver login",
            src=StringIO(login),
            dest=f"{STATE_DIR}/.cvspass",
            user=SERVICE_USER,
            group=SERVICE_USER,
            mode="0600",
            _sudo=True,
        )

    # The service is a oneshot started by the timer, so a changed executable
    # or configuration needs no restart; the next run picks it up.
    files.put(
        name="Install cvsgitrelay executable",
        src=str(local_binary),
        dest=executable,
        add_deploy_dir=False,
        user="root",
        group="root",
        mode="0755",
        _sudo=True,
    )
    files.put(
        name="Install cvsgitrelay configuration",
        src=str(source_config),
        dest=config_path,
        add_deploy_dir=False,
        user="root",
        group="root",
        mode="0644",
        _sudo=True,
    )

    # The first import can take hours, so the unit has no start timeout.
    # Exit status 75 means a temporary failure that the next run resolves.
    service_unit = dedent(
        f"""\
        [Unit]
        Description=Mirror CVS into Git with cvsgitrelay
        After=network-online.target
        Wants=network-online.target

        [Service]
        Type=oneshot
        User={SERVICE_USER}
        Group={SERVICE_USER}
        Environment=HOME={STATE_DIR}
        ExecStart={executable} {config_path} --repo {repo_dir}
        TimeoutStartSec=infinity

        # The sync only writes below its state directory.
        NoNewPrivileges=yes
        PrivateTmp=yes
        ProtectSystem=strict
        ProtectHome=yes
        ReadWritePaths={STATE_DIR}
        UMask=0027
        """
    )
    timer_unit = dedent(
        f"""\
        [Unit]
        Description=Run cvsgitrelay periodically

        [Timer]
        OnBootSec=2min
        # Count from the end of the previous run so syncs can never overlap.
        OnUnitInactiveSec={interval}

        [Install]
        WantedBy=timers.target
        """
    )

    files.put(
        name="Install cvsgitrelay systemd service",
        src=StringIO(service_unit),
        dest="/etc/systemd/system/cvsgitrelay.service",
        user="root",
        group="root",
        mode="0644",
        _sudo=True,
    )
    # A changed timer only takes effect after a restart of the timer.
    timer_path = "/etc/systemd/system/cvsgitrelay.timer"
    timer_changed = hashlib.sha256(timer_unit.encode()).hexdigest() != host.get_fact(
        Sha256File, path=timer_path, _sudo=True,
    )
    files.put(
        name="Install cvsgitrelay systemd timer",
        src=StringIO(timer_unit),
        dest=timer_path,
        user="root",
        group="root",
        mode="0644",
        _sudo=True,
    )

    # Starting the timer (not the service) keeps the deploy from waiting for a
    # long sync; an overdue timer starts the first sync right away.
    systemd.service(
        name="Enable and start the cvsgitrelay timer",
        service="cvsgitrelay.timer",
        running=True,
        restarted=timer_changed,
        enabled=True,
        daemon_reload=True,
        _sudo=True,
    )
