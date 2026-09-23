"""Exercise a real interactive shell and terminal-generated Ctrl-C (no API calls)."""
import errno
import fcntl
import struct
import termios
import os
import pty
import select
import shlex
import signal
import sys
import time

shell, launcher, phase, marker, temporary = sys.argv[1:]
pid, terminal = pty.fork()
if pid == 0:
    os.environ['TERM'] = 'dumb'
    os.environ['PS1'] = 'TERMA_PROMPT> '
    os.environ['PROMPT'] = 'TERMA_PROMPT> '
    args = [shell]
    if os.path.basename(shell) == 'bash':
        args += ['--noprofile', '--norc']
    if os.path.basename(shell) == 'zsh':
        args += ['-f']
    os.execv(shell, args + ['-i'])

transcript = b''
def receive_until(needle, timeout=8):
    global transcript
    deadline = time.monotonic() + timeout
    while needle not in transcript:
        if time.monotonic() >= deadline:
            raise AssertionError('terminal timed out waiting for %r: %r' % (needle, transcript))
        readable, _, _ = select.select([terminal], [], [], 0.05)
        if readable:
            try:
                chunk = os.read(terminal, 65536)
            except OSError as exc:
                if exc.errno == errno.EIO:
                    chunk = b''
                else:
                    raise
            if not chunk:
                raise AssertionError('terminal closed: %r' % transcript)
            transcript += chunk

def resize_until(rows, cols, timeout=8):
    global transcript
    needle = ('RESIZED:%d %d' % (rows, cols)).encode()
    deadline = time.monotonic() + timeout
    def arm():
        # SIGWINCH is edge-triggered and is not reliably queued: the kernel sends one
        # signal to the foreground process group at the instant the size changes, and a
        # delivery lost during the shell's terminal / process-group hand-off (seen under
        # zsh + wrapper-function activation) is never resent. Re-setting the same size is
        # a no-op that generates no new signal, so nudge the size first and then set the
        # target, forcing a fresh edge each attempt — exactly what a real terminal does.
        fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols + 1, 0, 0))
        fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))
    arm()
    next_arm = time.monotonic() + 0.3
    while needle not in transcript:
        now = time.monotonic()
        if now >= deadline:
            raise AssertionError('terminal timed out waiting for %r: %r' % (needle, transcript))
        if now >= next_arm:
            arm()
            next_arm = now + 0.3
        readable, _, _ = select.select([terminal], [], [], 0.05)
        if readable:
            chunk = os.read(terminal, 65536)
            if not chunk:
                raise AssertionError('terminal closed: %r' % transcript)
            transcript += chunk

try:
    receive_until(b'TERMA_PROMPT> ')
    transcript = b''
    command = shlex.quote(launcher)
    if os.environ.get('TERMA_PTY_SETUP'):
        command = '. ' + shlex.quote(os.environ['TERMA_PTY_SETUP']) + '; ' + command
    os.write(terminal, (command + '\n').encode())
    if phase == 'preparation':
        # Match the other readiness waits. First execution of freshly written
        # launchers can exceed 1.5s under macOS policy checks and a parallel test
        # run. This is a readiness handshake, not a launcher latency benchmark.
        deadline = time.monotonic() + 8
        while not os.path.exists(marker):
            if time.monotonic() >= deadline:
                raise AssertionError('preparation did not become ready: %r' % transcript)
            readable, _, _ = select.select([terminal], [], [], 0.01)
            if readable:
                transcript += os.read(terminal, 65536)
    else:
        receive_until(b'AGENT_READY')
        # The agent verifies all three descriptors are TTYs and reads our input.
        os.write(terminal, b'hello from terminal\n')
        receive_until(b'READ:hello from terminal')
        resize_until(41, 103)
    transcript = b''
    os.write(terminal, b'\x03')
    receive_until(b'TERMA_PROMPT> ')
    # Use a format placeholder so echoed shell input cannot satisfy the assertion.
    transcript = b''
    os.write(terminal, b"printf 'EXIT_STATUS:%s\\n' \"$?\"\n")
    receive_until(b'EXIT_STATUS:130')
    if phase == 'preparation' and os.path.exists(marker + '.agent'):
        raise AssertionError('cancelling preparation started the agent')
    if os.listdir(temporary):
        raise AssertionError('routing temp files survived Ctrl-C')
    transcript = b''
    os.write(terminal, b"printf 'TERMA_%s\\n' SHELL_ALIVE\n")
    receive_until(b'TERMA_SHELL_ALIVE')
finally:
    # The interactive shell owns its job groups. HUP tells it to terminate jobs.
    try:
        os.kill(pid, signal.SIGHUP)
    except ProcessLookupError:
        pass
    os.close(terminal)
    os.waitpid(pid, 0)
