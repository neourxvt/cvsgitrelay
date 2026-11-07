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

from pyinfra import host, logger
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
# The encoded remote secret is stored here when ``install`` gets one.
REMOTE_SECRET_PATH = f"{STATE_DIR}/remote.secret"


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


def repository_tables(config: dict, table: str) -> list[dict]:
    """Return ``table`` of the top-level settings and of every [[repository]].

    Top-level tables are the defaults of the entries, so deploy checks must
    look at both to see every value that can take effect.
    """
    tables = [config.get(table, {})]
    tables += [entry.get(table, {}) for entry in config.get("repository", [])]
    return tables


@deploy("Install cvsgitrelay")
def install(
    config_file: str | Path | None = None,
    repo_dir: str = f"{STATE_DIR}/repo.git",
    install_dir: str = "/opt/cvsgitrelay",
    interval: str = "1h",
    secrets: str | Path | None = None,
):
    """Install cvsgitrelay and sync CVS into a Git repository on a schedule.

    Args:
        config_file: Local TOML configuration; defaults to ``example.toml``.
            For a single repository its ``destination.repo`` is replaced by
            ``repo_dir``. A configuration with ``[[repository]]`` entries syncs
            them all in one run and keeps its own ``destination.repo`` paths,
            which must be absolute and below the state directory.
        repo_dir: Bare Git destination of a single-repository configuration,
            which must be below the state directory. It is created by the first
            sync. Ignored when the configuration has ``[[repository]]`` entries.
        install_dir: Directory containing the installed executable.
        interval: Time between the end of one sync and the start of the next.
        secrets: Local file with the encoded remote secret (the access token for
            an ``https://`` ``destination.remote``), as written by
            ``cvsgitrelay encode-secret``. It is installed readable by the
            service account only and passed to the service as
            ``--remote-secret``, overriding ``destination.remote_secret`` of
            every repository. A relative path is relative to the working directory of the deploy.
    """
    source_config = Path(config_file) if config_file is not None else DEFAULT_CONFIG
    with source_config.open("rb") as handle:
        config = tomllib.load(handle)

    multiple = bool(config.get("repository"))
    # The sync may only write below the state directory, and a relative path
    # would end up beside the configuration in /etc.
    if multiple:
        for destination in repository_tables(config, "destination"):
            repo = destination.get("repo")
            if repo is not None and not repo.startswith(f"{STATE_DIR}/"):
                raise ValueError(
                    f"destination.repo {repo!r} in {source_config} must be an "
                    f"absolute path below {STATE_DIR}"
                )
    # Fail before any change when the secret file is missing, instead of
    # installing a service whose every run fails on it.
    secret_file = Path(secrets).expanduser() if secrets is not None else None
    if secret_file is not None and not secret_file.is_file():
        raise FileNotFoundError(f"cvsgitrelay secret file not found: {secret_file}")
    # The secret is only used for pushing, which needs a remote. Without one it
    # would be silently ignored, so say so.
    if secret_file is not None and not any(
        destination.get("remote") for destination in repository_tables(config, "destination")
    ):
        logger.warning(
            "cvsgitrelay: a secret is deployed but %s sets no destination.remote, "
            "so nothing is pushed",
            source_config,
        )

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
    # Every repository can have its own CVSROOT; one login line per distinct one.
    logins = []
    for source in repository_tables(config, "source"):
        login = pserver_login_entry(source.get("cvsroot", ""))
        if login is not None and login not in logins:
            logins.append(login)
    if logins:
        files.put(
            name="Store the anonymous CVS pserver login",
            src=StringIO("".join(logins)),
            dest=f"{STATE_DIR}/.cvspass",
            user=SERVICE_USER,
            group=SERVICE_USER,
            mode="0600",
            _sudo=True,
        )

    # The token must be readable by the service only. The encoding is just
    # obfuscation, so the file permissions are the actual protection.
    if secret_file is not None:
        files.put(
            name="Install the cvsgitrelay remote secret",
            src=str(secret_file),
            dest=REMOTE_SECRET_PATH,
            add_deploy_dir=False,
            user=SERVICE_USER,
            group=SERVICE_USER,
            mode="0400",
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

    # --repo names one destination, so repositories of a multi-repository
    # configuration use the paths of the configuration itself.
    exec_start = f"{executable} {config_path}"
    if not multiple:
        exec_start += f" --repo {repo_dir}"
    if secret_file is not None:
        exec_start += f" --remote-secret {REMOTE_SECRET_PATH}"

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
        ExecStart={exec_start}
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
