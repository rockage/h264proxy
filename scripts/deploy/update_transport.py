#!/usr/bin/env python3
"""Root-only gateway transport update; preserves key, unit and routing config."""
import argparse
import datetime
import hashlib
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import time
import uuid

BIN = Path('/opt/h264proxy/bin/proxy')
ENV = Path('/etc/h264proxy/gateway.env')
UNIT = 'h264proxy.service'


def run(*args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=20, **kwargs)


def atomic_write(path, data, mode, uid, gid):
    fd, name = tempfile.mkstemp(prefix='.' + path.name + '-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            os.fchmod(output.fileno(), mode)
            os.fchown(output.fileno(), uid, gid)
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def restore_labels():
    if shutil.which('restorecon'):
        run('restorecon', str(BIN), str(ENV))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('candidate', type=Path)
    parser.add_argument('sha256')
    parser.add_argument('--high', type=int, default=1048576)
    parser.add_argument('--low', type=int, default=524288)
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('must run as root on gateway')
    if not 32768 <= args.high <= 8388608 or not 0 <= args.low < args.high:
        parser.error('invalid transport watermarks')
    candidate = args.candidate.read_bytes()
    if hashlib.sha256(candidate).hexdigest() != args.sha256:
        raise RuntimeError('candidate SHA256 mismatch')
    if not BIN.is_file() or not ENV.is_file():
        raise RuntimeError('existing gateway installation required')
    run('systemctl', 'is-active', '--quiet', UNIT)
    command = run('systemctl', 'show', UNIT, '-p', 'ExecStart', '--value').stdout
    if '-dc-high-water' in command or '-dc-low-water' in command:
        raise RuntimeError('unit has explicit watermark flags that would override EnvironmentFile')
    # An invalid role exits before networking. The new flags must parse and
    # validate successfully before reaching the expected role validation.
    probe = subprocess.run([str(args.candidate), '-role', 'invalid', '-dc-high-water', str(args.high),
                            '-dc-low-water', str(args.low)], capture_output=True, text=True, timeout=10)
    if probe.returncode != 2 or '-role must be gateway or client' not in probe.stderr:
        raise RuntimeError('candidate parameter validation failed: ' + probe.stderr)
    old_env = ENV.read_bytes()
    meta = ENV.stat()
    desired = {'H264_DC_HIGH_WATER': str(args.high), 'H264_DC_LOW_WATER': str(args.low)}
    # Keep every unrelated line, including signaling, ICE and any custom keys.
    lines = old_env.decode().splitlines(keepends=True)
    kept = [line for line in lines if line.partition('=')[0].strip() not in desired]
    updated = ''.join(kept)
    if updated and not updated.endswith('\n'):
        updated += '\n'
    updated += ''.join(f'{key}={value}\n' for key, value in desired.items())
    if args.check:
        print('Preflight passed: candidate hash/flags, active unit, environment preservation.')
        print('Planned gateway send high/low:', args.high, args.low)
        return
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = Path('/opt/h264proxy/backups') / ('transport-' + stamp + '-' + uuid.uuid4().hex[:8])
    backup.mkdir(parents=True, mode=0o700)
    shutil.copy2(BIN, backup / 'proxy')
    shutil.copy2(ENV, backup / 'gateway.env')
    print('Backup:', backup, flush=True)
    try:
        atomic_write(BIN, candidate, 0o755, 0, 0)
        atomic_write(ENV, updated.encode(), stat.S_IMODE(meta.st_mode), meta.st_uid, meta.st_gid)
        restore_labels()
        run('systemctl', 'restart', UNIT)
        deadline = time.monotonic() + 15
        pid = None
        while time.monotonic() < deadline:
            run('systemctl', 'is-active', '--quiet', UNIT)
            pid = run('systemctl', 'show', UNIT, '-p', 'MainPID', '--value').stdout.strip()
            sockets = run('ss', '-lntp').stdout
            if len([line for line in sockets.splitlines() if f'pid={pid},' in line]) >= 2:
                break
            time.sleep(.25)
        else:
            raise RuntimeError('gateway signaling/ICE-TCP listeners not ready')
        actual_hash = hashlib.sha256(Path(f'/proc/{pid}/exe').read_bytes()).hexdigest()
        if actual_hash != args.sha256:
            raise RuntimeError('running executable differs from candidate')
        process_env = dict(item.split(b'=', 1) for item in Path(f'/proc/{pid}/environ').read_bytes().split(b'\0') if b'=' in item)
        if any(process_env.get(key.encode()) != value.encode() for key, value in desired.items()):
            raise RuntimeError('running process did not inherit requested thresholds')
        print('Deployed SHA256:', actual_hash)
        print('Gateway active, PID:', pid)
        print('Effective process environment:', desired)
        print('Stream receive window left unchanged; key/unit/firewall left unchanged.')
    except BaseException:
        # Restore both files even after a partial update. Keep the backup for
        # inspection; never modify the production shared key during rollback.
        prior = (backup / 'proxy').read_bytes()
        atomic_write(BIN, prior, 0o755, 0, 0)
        atomic_write(ENV, old_env, stat.S_IMODE(meta.st_mode), meta.st_uid, meta.st_gid)
        restore_labels()
        run('systemctl', 'restart', UNIT)
        run('systemctl', 'is-active', '--quiet', UNIT)
        print('Update failed; prior binary/environment restored:', backup, flush=True)
        raise


if __name__ == '__main__':
    main()
