#!/usr/bin/env python3
"""Start a command in its own session (macOS has no setsid), detached from the caller's terminal and descriptors.

Writes "<pid> <start time>" to --pid-file before exec, so a later stop can tell the process from a reused pid.
Exits non-zero when the command cannot be started.
"""
import argparse
import os
import subprocess
import sys
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pid-file', required=True)
    parser.add_argument('--log', required=True)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        parser.error('a command is required after --')
    os.umask(0o077)
    for path in (args.pid_file, args.log):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
    # The parent waits until the child has exec'd (the pipe closes on exec) or reports why it could not.
    read_fd, write_fd = os.pipe()
    pid = os.fork()
    if pid:
        os.close(write_fd)
        with os.fdopen(read_fd) as ready:
            result = ready.read()
        if result:
            print(result, file=sys.stderr, end='')
            os.waitpid(pid, 0)
            return 1
        return 0
    os.close(read_fd)
    try:
        os.setsid()
        os.closerange(3, write_fd)
        os.closerange(write_fd + 1, os.sysconf('SC_OPEN_MAX'))
        with open(os.devnull, 'rb') as stdin, open(args.log, 'ab', buffering=0) as log:
            os.dup2(stdin.fileno(), 0)
            os.dup2(log.fileno(), 1)
            os.dup2(log.fileno(), 2)
        start = subprocess.check_output(['ps', '-o', 'lstart=', '-p', str(os.getpid())], text=True)
        Path(args.pid_file).write_text(f'{os.getpid()} {" ".join(start.split())}\n')
        os.set_inheritable(write_fd, False)
        os.execvp(command[0], command)
    except Exception as exc:  # tell the caller why the start failed
        os.write(write_fd, f'detach: {exc}\n'.encode())
        os._exit(1)


if __name__ == '__main__':
    sys.exit(main())
