"""Serialize access to one disposable test environment (macOS/Linux)."""
import argparse
from contextlib import contextmanager
import fcntl
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import time


DEFAULT_ROOT = "~/.cache/mmx-testing"
LOCK_FD = "MMX_TEST_LOCK_FD"
# The directory where the job slots of one hetzner.py or incus.py run share
# one build of MMX and of the Go test programs.
SHARED_BUILD = "MMX_TEST_SHARED_BUILD"


def root():
    return Path(os.environ.get("MMX_TEST_ROOT", DEFAULT_ROOT)).expanduser().resolve()


def environment(**overrides):
    """Keep a caller's development paths out of isolated integration projects."""
    env = dict(os.environ)
    env.pop("MMX_STATE_DIR", None)
    env.pop("MMX_RUNNER_DIR", None)
    env.update(overrides)
    return env


def inherited_fd(directory):
    """Accept a nested command's descriptor only for this environment's lock."""
    try:
        fd = int(os.environ[LOCK_FD])
        if os.path.samestat(os.fstat(fd), (directory / ".test.lock").stat()):
            return fd
    except (KeyError, ValueError, OSError):
        pass
    return None


@contextmanager
def locked(directory):
    directory = Path(directory).expanduser().resolve()
    directory.mkdir(parents=True, exist_ok=True)
    inherited = inherited_fd(directory)
    if inherited is not None:
        # A nested build shares the same open file description, so this does
        # not compete with its parent or release the parent's lock on return.
        fcntl.flock(inherited, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield inherited
        return

    with (directory / ".test.lock").open("a+") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            handle.seek(0)
            owner = handle.read(4096).strip() or "another test command"
            print(f"Waiting for test environment {directory}; held by {owner}",
                  file=sys.stderr, flush=True)
            fcntl.flock(handle, fcntl.LOCK_EX)
        handle.seek(0)
        handle.truncate()
        handle.write(f"PID {os.getpid()}: {shlex.join(sys.argv)}\n")
        handle.flush()
        previous = os.environ.get(LOCK_FD)
        os.environ[LOCK_FD] = str(handle.fileno())
        try:
            yield handle.fileno()
        finally:
            if previous is None:
                os.environ.pop(LOCK_FD, None)
            else:
                os.environ[LOCK_FD] = previous
        # Closing releases the OS lock. Keep the file: unlinking it could let
        # another process lock a different inode while an existing waiter wakes.


def lock_holder(directory):
    """Return the process ID and command of the test command that holds this
    environment's lock, or None when no command holds it."""
    path = Path(directory).expanduser().resolve() / ".test.lock"
    try:
        handle = path.open("r+")
    except FileNotFoundError:
        return None
    with handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            owner = re.match(r"PID (\d+): (.+)", handle.read(4096).strip())
            if not owner:
                raise RuntimeError(f"{path} is held and does not name the command that holds it")
            return int(owner.group(1)), owner.group(2)
        # Nothing held the lock; closing the file gives it back.
        return None


def process_tree(pid):
    """Return pid and every process it started, directly or through others."""
    listing = subprocess.run(["ps", "-eo", "pid=,ppid="], check=True, text=True,
                             stdout=subprocess.PIPE).stdout
    started_by = {}
    for line in listing.splitlines():
        child, parent = (int(field) for field in line.split())
        started_by.setdefault(parent, []).append(child)
    tree, waiting = [], [pid]
    while waiting:
        current = waiting.pop()
        tree.append(current)
        waiting.extend(started_by.get(current, []))
    return tree


def still_running(pids):
    """Return the given processes that still run; a process that has ended and
    waits for its parent to collect it does not count."""
    listing = subprocess.run(["ps", "-o", "pid=,stat=", "-p", ",".join(str(pid) for pid in pids)],
                             text=True, stdout=subprocess.PIPE).stdout
    return [int(line.split()[0]) for line in listing.splitlines() if not line.split()[1].startswith("Z")]


def stop_run(directory, seconds_until_kill=10):
    """End the test command that holds this environment's lock, with every
    process it started. Return its command, or None when no command held the
    lock. Processes of other test environments are never touched: the lock
    file names the one process whose tree is ended."""
    holder = lock_holder(directory)
    if holder is None:
        return None
    pid, command = holder
    now = subprocess.run(["ps", "-ww", "-o", "args=", "-p", str(pid)], text=True,
                         stdout=subprocess.PIPE).stdout.strip()
    if command not in now:
        raise RuntimeError(
            f"the lock of {directory} names PID {pid} ({command}), but that process is now "
            f"{now or 'gone'}; a command it started still holds the lock. End that command by its process ID.")
    tree = process_tree(pid)
    if os.getpid() in tree:
        raise RuntimeError(f"this command was started by the test command it would end (PID {pid})")
    for signal_number, seconds in ((signal.SIGTERM, seconds_until_kill), (signal.SIGKILL, 5)):
        for process in tree:
            try:
                os.kill(process, signal_number)
            except ProcessLookupError:
                pass
        deadline = time.monotonic() + seconds
        while still_running(tree) and time.monotonic() < deadline:
            time.sleep(0.1)
        if not still_running(tree):
            return command
    raise RuntimeError(f"processes of PID {pid} ({command}) did not end: {still_running(tree)}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=root())
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command[:1] == ["--"]:
        command = command[1:]
    if not command:
        parser.error("provide a command after --")
    with locked(args.root) as fd:
        os.set_inheritable(fd, True)
        os.execvp(command[0], command)


if __name__ == "__main__":
    main()
