"""Exercise the real installer transaction with isolated command fixtures.

No root privileges, network, service or host-firewall changes are required.
Run: python3 -m unittest discover -s tests/installer -v
"""
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / 'install.sh'
SHA = 'a' * 40

FAKE_COMMAND = r'''
import json, os, pathlib, shutil, sys
root = pathlib.Path(os.environ['TEST_ROOT'])
state_file = root / 'state.json'
s = json.loads(state_file.read_text())
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
s.setdefault('calls', []).append([name] + args)
def save(): state_file.write_text(json.dumps(s))
def done(code=0, text=''):
    if text: print(text)
    save()
    raise SystemExit(code)
def option(prefix, default=''):
    return next((a[len(prefix):] for a in args if a.startswith(prefix)), default)
if name == 'go':
    if args == ['version']: done(text='go version go1.27.3 linux/' + s['arch'])
    if args[:1] == ['env']: done(text='linux\n' + s['arch'])
    if args[:1] == ['build']:
        if s.get('fail_build'): done(1, 'simulated compiler failure')
        dest = pathlib.Path(args[args.index('-o') + 1])
        if args[-1].endswith('/genkey'):
            code = '#!' + sys.executable + '\nimport os,pathlib,sys\np=pathlib.Path(sys.argv[sys.argv.index("-out")+1]);p.write_bytes(os.urandom(32));p.chmod(0o600)\n'
        else: code = '#!/bin/sh\n# binary-version:' + s.get('version', 'v1') + '\nexit 0\n'
        dest.write_text(code); dest.chmod(0o755); done()
if name == 'curl':
    url = next(a for a in args if a.startswith(('https://', 'http://')))
    dest = pathlib.Path(args[args.index('-o') + 1]) if '-o' in args else None
    if url.startswith('http://127.0.0.1:'):
        binary = root / 'opt/bin/proxy'
        is_new = binary.exists() and 'binary-version:v2' in binary.read_text()
        if not s.get('active') or (s.get('fail_health') and is_new): done(22)
        data = json.dumps({'sdp': {'type':'offer', 'sdp':'m=video 9 UDP/TLS/RTP/SAVPF 96\na=rtpmap:96 H264/90000'}}).encode()
    elif 'api.ipify.org' in url or 'icanhazip.com' in url: data = b'8.8.8.8'
    elif '/commits/' in url: data = json.dumps({'sha': 'a' * 40}).encode()
    elif 'codeload.github.com' in url: data = (root / 'source.tar.gz').read_bytes()
    elif '?mode=json' in url:
        digest = '0' * 64 if s.get('bad_checksum') else hashlib_sha(root / 'go.tar.gz')
        data = json.dumps([{'version':'go1.27.3', 'stable':True, 'files':[{'filename':'go1.27.3.linux-'+s['arch']+'.tar.gz', 'os':'linux', 'arch':s['arch'], 'kind':'archive', 'sha256':digest}]}]).encode()
    elif '/dl/go1.27.3.linux-' in url: data = (root / 'go.tar.gz').read_bytes()
    else: done(22, 'unexpected download URL')
    if dest: dest.write_bytes(data)
    else: sys.stdout.buffer.write(data)
    done()
if name == 'systemctl':
    if args[:1] == ['is-active']: done(0 if s.get('active') else 3)
    if args[:1] == ['is-enabled']: done(0 if s.get('enabled') else 1)
    if args[:1] == ['show']: done(text=str(s.get('pid',0)))
    if args[:1] == ['enable']: s['enabled'] = True
    if args[:1] == ['disable']: s['enabled'] = False
    if args[:1] == ['stop']: s['active'] = False; s['pid'] = 0
    if args[:1] == ['restart']: s['active'] = True; s['pid'] = s.get('counter',100) + 1; s['counter'] = s['pid']
    done()
if name == 'systemd-analyze': done(1 if s.get('fail_verify') else 0)
if name == 'ss':
    if s.get('port_conflict'): done(text='LISTEN 0 10 0.0.0.0:9300 users:(("unrelated",pid=999,fd=3))')
    done()
if name == 'ip': done(text='default via 1.1.1.1 dev eth0')
if name == 'firewall-cmd':
    if '--state' in args: done(0 if s.get('firewall') == 'firewalld' else 1)
    if any(a.startswith('--get-zone-of-interface') for a in args) or '--get-default-zone' in args: done(text='public')
    mode = 'permanent' if '--permanent' in args else 'runtime'
    rules = set(s.get('rules', []))
    query, add, remove = option('--query-port='), option('--add-port='), option('--remove-port=')
    if query: done(0 if mode+':'+query in rules else 1)
    if add: rules.add(mode+':'+add)
    if remove: rules.discard(mode+':'+remove)
    s['rules'] = sorted(rules); done()
if name == 'ufw':
    rules = s.setdefault('ufw_rules', [])
    if args == ['status']: done(text='Status: '+('active' if s.get('firewall') == 'ufw' else 'inactive')+'\n'+'\n'.join(rules))
    if args[:1] == ['allow']: rules.append(args[1]+' ALLOW IN Anywhere')
    if args[:3] == ['--force','delete','allow']:
        s['ufw_rules'] = [r for r in rules if r != args[3]+' ALLOW IN Anywhere']
    done()
if name == 'sha256sum':
    digest, filename = sys.stdin.read().strip().split(None, 1)
    good = hashlib_sha(pathlib.Path(filename.strip())) == digest
    done(0 if good else 1, 'checksum OK' if good else 'checksum FAILED')
if name == 'install':
    mode, directory, paths, i = 0o755, False, [], 0
    while i < len(args):
        a = args[i]
        if a == '-d': directory = True
        elif a in ('-m','-o','-g'):
            if a == '-m': mode = int(args[i+1],8)
            i += 1
        else: paths.append(pathlib.Path(a))
        i += 1
    if directory:
        for p in paths: p.mkdir(parents=True,exist_ok=True);p.chmod(mode)
    else: shutil.copyfile(paths[0],paths[1]);paths[1].chmod(mode)
    done()
if name in ('apt-get','dnf','yum','getent','groupadd','useradd','chown','restorecon','flock','sleep','journalctl'): done()
raise SystemExit('unhandled fake command '+name)
'''
# The fixture executable is copied into the Go archive too; use only stdlib.
FAKE_COMMAND = FAKE_COMMAND.replace("def option(prefix, default=''):", "def hashlib_sha(path):\n    import hashlib\n    return hashlib.sha256(path.read_bytes()).hexdigest()\ndef option(prefix, default=''):")


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='h264proxy-installer-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'fakebin'
        self.bin.mkdir()
        executable = '#!' + sys.executable + '\n' + FAKE_COMMAND
        fake = self.bin / '_fake'
        fake.write_text(executable)
        fake.chmod(0o755)
        for name in ('go', 'curl', 'systemctl', 'systemd-analyze', 'ss', 'ip', 'firewall-cmd', 'ufw', 'install', 'apt-get', 'getent', 'groupadd', 'useradd', 'chown', 'restorecon', 'flock', 'sleep', 'journalctl', 'sha256sum'):
            (self.bin / name).symlink_to(fake)
        with tarfile.open(self.root / 'source.tar.gz', 'w:gz') as tf:
            for name, body in {'go.mod':'module fixture\n\ngo 1.24.7\n', 'cmd/proxy/main.go':'package main\n', 'cmd/genkey/main.go':'package main\n'}.items():
                data = body.encode(); member = tarfile.TarInfo('source-root/'+name);member.size=len(data);member.mode=0o644;tf.addfile(member,io.BytesIO(data))
        with tarfile.open(self.root / 'go.tar.gz', 'w:gz') as tf:
            data = executable.encode(); member=tarfile.TarInfo('go/bin/go');member.size=len(data);member.mode=0o755;tf.addfile(member,io.BytesIO(data))
        self.state_file = self.root / 'state.json'
        self.write_state({'arch':'arm64','firewall':'firewalld','active':False,'enabled':False,'version':'v1','rules':[]})
        self.driver = self.root / 'driver.sh'
        self.driver.write_text('''#!/usr/bin/env bash
source "$INSTALLER"
SERVICE=fixture.service
APP_USER=nobody
INSTALL_DIR="$TEST_ROOT/opt"
CONFIG_DIR="$TEST_ROOT/config"
STATE_DIR="$TEST_ROOT/state"
UNIT_DIR="$TEST_ROOT/units"
LOCK_FILE="$TEST_ROOT/install.lock"
check_system() { GO_ARCH="$(python3 -c 'import json,os;print(json.load(open(os.environ["TEST_ROOT"]+"/state.json"))["arch"])')"; }
# Exclude host-wide Go from this isolated fixture, including on Linux CI.
eval "$(declare -f usable_go | sed '1s/usable_go/fixture_usable_go/')"
usable_go() {
  case "$1" in
    "$TEST_ROOT"/*|"$WORK_DIR"/*) fixture_usable_go "$1" ;;
    *) return 1 ;;
  esac
}
main "$@"
''')
        self.env = dict(os.environ, PATH=str(self.bin)+':/usr/bin:/bin', TEST_ROOT=str(self.root), INSTALLER=str(INSTALLER))
        self.env.pop('PUBLIC_IP',None);self.env.pop('PORT',None)

    def state(self): return json.loads(self.state_file.read_text())
    def write_state(self, state): self.state_file.write_text(json.dumps(state))
    def set_state(self, **values):
        state=self.state();state.update(values);self.write_state(state)
    def run_installer(self, *args, success=True):
        result=subprocess.run(['bash',str(self.driver),*args],env=self.env,capture_output=True,text=True,timeout=60)
        if success: self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        else: self.assertNotEqual(result.returncode,0,result.stdout+result.stderr)
        return result
    def install(self, **kw): return self.run_installer('--public-ip','8.8.8.8',**kw)
    def key(self): return (self.root/'config/key.bin').read_bytes()
    def proxy(self): return (self.root/'opt/bin/proxy').read_bytes()
    def assert_healthy(self):
        self.assertTrue(self.state()['active']);self.assertTrue(self.state()['enabled'])
        self.assertEqual(len(self.key()),32)
        self.assertEqual((self.root/'config/key.bin').stat().st_mode&0o777,0o640)
        self.assertEqual((self.root/'opt/bin/proxy').stat().st_mode&0o777,0o755)

    def test_fresh_and_repeat_keep_key_and_configuration(self):
        self.install();self.assert_healthy()
        first_key=self.key();first_env=(self.root/'config/gateway.env').read_bytes()
        self.set_state(version='v2')
        result=self.run_installer()
        self.assert_healthy();self.assertEqual(self.key(),first_key)
        self.assertEqual((self.root/'config/gateway.env').read_bytes(),first_env)
        self.assertIn(b'binary-version:v2',self.proxy())
        self.assertIn('INSTALLED',result.stdout)
        self.assertEqual(len(self.state()['rules']),4)
        self.assertTrue((self.root/f'opt/sources/{SHA}/go.mod').exists())

    def test_download_private_toolchain_and_checksum_failure(self):
        # No executable Go in PATH: only the verified archive can provide it.
        (self.bin/'go').unlink()
        self.install();self.assert_healthy()
        self.assertTrue((self.root/'opt/toolchains/current/bin/go').is_file())
        first_key=self.key();first_binary=self.proxy()
        # Force another archive selection, then corrupt its advertised checksum.
        shutil.rmtree(self.root/'opt/toolchains')
        self.set_state(bad_checksum=True,version='v2')
        self.install(success=False)
        self.assertEqual(self.key(),first_key);self.assertEqual(self.proxy(),first_binary)
        self.assertTrue(self.state()['active'])

    def test_failed_health_rolls_back_and_removes_only_new_rules(self):
        self.install();first_key=self.key();first_binary=self.proxy()
        first_env=(self.root/'config/gateway.env').read_bytes()
        first_unit=(self.root/'units/fixture.service').read_bytes()
        rules=self.state()['rules']
        self.set_state(version='v2',fail_health=True)
        result=self.run_installer('--public-ip','1.1.1.1','--port','9400',success=False)
        self.assertNotIn('INSTALLED:',result.stdout)
        self.assertEqual(self.key(),first_key);self.assertEqual(self.proxy(),first_binary)
        self.assertEqual((self.root/'config/gateway.env').read_bytes(),first_env)
        self.assertEqual((self.root/'units/fixture.service').read_bytes(),first_unit)
        self.assertEqual(self.state()['rules'],rules)
        self.assertTrue(self.state()['active']);self.assertTrue(self.state()['enabled'])

    def test_compile_failure_and_conflicting_port_leave_old_service(self):
        self.install();first_key=self.key();first_binary=self.proxy()
        self.set_state(version='v2',fail_build=True)
        self.install(success=False)
        self.assertEqual(self.proxy(),first_binary);self.assertEqual(self.key(),first_key)
        self.set_state(fail_build=False,port_conflict=True)
        result=self.install(success=False)
        self.assertIn('occupied',result.stderr)
        self.assertEqual(self.proxy(),first_binary);self.assertTrue(self.state()['active'])

    def test_fresh_unit_validation_failure_is_not_enabled(self):
        self.set_state(fail_verify=True)
        self.install(success=False)
        self.assertFalse(self.state()['enabled']);self.assertFalse(self.state()['active'])
        self.assertFalse((self.root/'units/fixture.service').exists())
        self.assertEqual(self.state()['rules'],[])

    def test_ufw_no_firewall_and_privileged_port(self):
        self.set_state(firewall='ufw',ufw_rules=['9300/tcp ALLOW OUT Anywhere'])
        self.install();self.assert_healthy()
        self.assertIn('9300/tcp ALLOW IN Anywhere',self.state()['ufw_rules'])
        self.assertIn('9300/udp ALLOW IN Anywhere',self.state()['ufw_rules'])
        rules=self.state()['ufw_rules'][:]
        self.run_installer('--port','443','--no-firewall')
        self.assertEqual(self.state()['ufw_rules'],rules)
        unit=(self.root/'units/fixture.service').read_text()
        self.assertIn('AmbientCapabilities=CAP_NET_BIND_SERVICE',unit)
        self.assertIn('AF_NETLINK',unit)

    def test_bad_inputs_do_not_install_and_help_works_from_stdin(self):
        for args in [('--port','0'),('--port','65536'),('--port','9;echo x'),('--ref','../oops'),('--public-ip','127.0.0.1')]:
            self.run_installer(*args,success=False)
        self.assertFalse((self.root/'opt/bin/proxy').exists())
        result=subprocess.run(['bash','-s','--','--help'],input=INSTALLER.read_text(),capture_output=True,text=True,timeout=5)
        self.assertEqual(result.returncode,0,result.stderr);self.assertIn('Usage:',result.stdout)

    def test_auto_address_and_invalid_existing_key(self):
        self.run_installer('--port','9555','--no-firewall')
        env=(self.root/'config/gateway.env').read_text()
        self.assertIn('PUBLIC_IP=8.8.8.8',env);self.assertIn('UDP_PORT=9555',env)
        self.assertEqual(self.state()['rules'],[])
        key=self.root/'config/key.bin'
        key.write_bytes(b'invalid-existing-key')
        result=self.run_installer(success=False)
        self.assertIn('refusing to replace',result.stderr)
        self.assertEqual(key.read_bytes(),b'invalid-existing-key')
        self.assertTrue(self.state()['active'])


if __name__ == '__main__': unittest.main()
