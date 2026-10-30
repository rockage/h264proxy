#!/usr/bin/env python3
"""ABBA full-system comparison. No explicit proxy; same binary/VPS/ICE-TCP.

Run via compare_tun.sh. Results persist after each phase, including failures.
"""
import concurrent.futures
import argparse
import datetime
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import statistics
import subprocess
import sys
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
SERVICE = os.environ.get('H264_TUN_SERVICE', '')
ENDPOINT = os.environ.get('H264_ENDPOINT', '')
SSH = os.environ.get('H264_SSH', '')
KEY = os.environ.get('H264_KEY', str(ROOT / 'build/proxy-vps.key'))
SOCKS = os.environ.get('H264_SOCKS', '127.0.0.1:1080')
DEVICE = os.environ.get('H264_TUN_DEVICE', 'utun123')
STATE = '/Library/Application Support/H264Proxy/tun-state.json'
SIZE = 4 * 1024 * 1024
PERFORMANCE = {name: int(os.environ.get(env, '0')) for name, env in (
    ('dc-high-water', 'H264_DC_HIGH_WATER'), ('dc-low-water', 'H264_DC_LOW_WATER'),
    ('stream-receive-window', 'H264_STREAM_RECEIVE_WINDOW'))}


def run(args, timeout=20, check=True):
    return subprocess.run(args, capture_output=True, text=True, timeout=timeout, check=check)


def route(address):
    text = run(['/sbin/route', '-n', 'get', address]).stdout
    return dict(re.findall(r'^\s*(\w+):\s*(.*?)\s*$', text, re.M))


def preflight():
    if sys.platform != 'darwin' or not SERVICE or not SSH:
        raise RuntimeError('需要 macOS、H264_TUN_SERVICE 和 H264_SSH。')
    if os.environ.get('H264_ICE_TRANSPORT', 'udp') != 'tcp':
        raise RuntimeError('本轮固定 ICE-TCP；请设置 H264_ICE_TRANSPORT=tcp。')
    ip = str(ipaddress.IPv4Address(ENDPOINT.rsplit(':', 1)[0]))
    physical = route('default')['interface']
    for target in (ip, '8.8.8.8'):
        got = route(target)['interface']
        if physical.startswith('utun') or got != physical:
            raise RuntimeError(f'{target} 当前经过 {got}；对比前请关闭其他 VPN/TUN。脚本可独立运行到结束，无需维持聊天连接。')
    for name in ('proxy', 'tools/tun2socks', 'tools/sing-box'):
        if not os.access(ROOT / 'build' / name, os.X_OK):
            raise RuntimeError(f'缺少 build/{name}；先构建两个前端。')
    if not Path(KEY).is_file():
        raise RuntimeError('找不到密钥文件。')
    host, port = SOCKS.rsplit(':', 1)
    with socket.socket() as listener:
        listener.bind((host, int(port)))
    if DEVICE in run(['/sbin/ifconfig', '-l']).stdout.split():
        raise RuntimeError(f'{DEVICE} 已存在；请先停止旧客户端。')
    return ip


def dns_query(name, qtype='A', server='127.0.0.1'):
    started = time.monotonic()
    # Apple's bundled BIND 9.10 predates the HTTPS/SVCB mnemonics. Unknown
    # mnemonics become extra names with implicit A queries, not an error.
    wire_type = {'HTTPS': 'TYPE65', 'SVCB': 'TYPE64'}.get(qtype, qtype)
    r = run(['/usr/bin/dig', '@' + server, name, wire_type, '+tries=1', '+time=10'], check=False)
    status = re.search(r'status: (\w+)', r.stdout)
    answers = re.search(r'ANSWER: (\d+)', r.stdout)
    ok = r.returncode == 0 and status and status[1] == 'NOERROR'
    value = {'name': name, 'type': qtype, 'server': server, 'seconds': time.monotonic()-started,
             'ok': bool(ok), 'answers': int(answers[1]) if answers else None}
    return value


def download(directory, name):
    path = directory / (name + '.bin')
    try:
        r = run(['curl', '--proxy', '', '--noproxy', '*', '--http1.1', '-4',
                 '--connect-timeout', '15', '--max-time', '90', '--fail', '--silent', '--show-error',
                 '--output', str(path), '--write-out', '%{json}',
                 f'https://speed.cloudflare.com/__down?bytes={SIZE}'], timeout=100, check=False)
        data = json.loads(r.stdout)
        received = path.stat().st_size if path.exists() else 0
        valid = r.returncode == 0 and received == SIZE and int(data['size_download']) == SIZE
        result = {'ok': valid, 'bytes': received,
                'seconds': data['time_total'], 'dns_seconds': data['time_namelookup'],
                'ttfb_seconds': data['time_starttransfer'], 'tls_seconds': data['time_appconnect'],
                'Mbps': data['size_download']*8/max(data['time_total'], .000001)/1e6,
                # Cloudflare generates each response; hashes are recorded, not expected equal.
                'sha256': hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None}
        if not valid:
            result['error'] = r.stderr.strip() or 'response length mismatch'
        return result
    finally:
        path.unlink(missing_ok=True)


def stop(process):
    if process.poll() is None:
        # Keep the invoking terminal: macOS sudo timestamps are TTY-scoped.
        # sudo forwards TERM to the command (or execs it directly).
        run(['sudo', '-n', '/bin/kill', '-TERM', str(process.pid)], check=False)
        try:
            process.wait(timeout=20)
        except subprocess.TimeoutExpired:
            raise RuntimeError('客户端未及时停止；保留恢复记录，请先停止原客户端再运行 -tun-recover。')
    if run(['sudo', '-n', 'test', '-f', STATE], check=False).returncode == 0:
        run(['sudo', '-n', str(ROOT/'build/proxy'), '-tun-recover'], timeout=40)


def phase(directory, number, backend, ip):
    print(f'[{number}/4] {backend}: 启动、DNS、串行/并发下载、恢复', flush=True)
    run(['sudo', '-n', 'true'])
    run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', SSH,
         'systemctl restart h264proxy.service'])
    prefix = directory / f'{number}-{backend}'
    console = prefix.with_suffix('.console.log')
    if console.exists():
        console.rename(prefix.with_suffix(f'.console-before-resume-{time.time_ns()}.log'))
    args = ['sudo', '-n', str(ROOT/'build/proxy'), '-role', 'client', '-net',
            '-ice-transport', 'tcp', '-http', ENDPOINT, '-stun', '', '-key', KEY,
            '-socks', SOCKS, '-connect-timeout', '60s', '-tun', '-tun-backend', backend,
            '-tun-service', SERVICE, '-tun-device', DEVICE,
            '-tun2socks', str(ROOT/'build/tools/tun2socks'),
            '-sing-box', str(ROOT/'build/tools/sing-box'),
            '-report', str(prefix.with_suffix('.mux.json'))]
    for name, value in PERFORMANCE.items():
        args.extend(['-' + name, str(value)])
    started = time.monotonic()
    result = {'backend': backend, 'phase': number}
    with console.open('w') as log:
        process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = started + 120
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError(f'客户端提前退出，见 {console}')
                if '"msg":"tun_ready"' in console.read_text():
                    break
                time.sleep(.2)
            else:
                raise RuntimeError(f'TUN 启动超时，见 {console}')
            result['startup_seconds'] = time.monotonic()-started
            result['transport_route'] = route(ip)
            result['test_route'] = route('1.1.1.1')
            if result['transport_route']['interface'].startswith('utun') or result['test_route']['interface'] != DEVICE:
                raise RuntimeError('路由验证失败，拒绝记录旁路或套娃结果。')
            egress = run(['curl', '--proxy', '', '--noproxy', '*', '-4', '--fail', '--silent',
                          '--show-error', '--max-time', '20', 'https://api.ipify.org'], timeout=25).stdout.strip()
            result['egress'] = egress
            if egress != ip:
                raise RuntimeError(f'出口 {egress} 与 VPS {ip} 不同，停止比较。')
            result['dns'] = [dns_query(domain) for domain in ('www.youtube.com', 'i.ytimg.com', 'example.com') for _ in range(3)]
            result['ipv4_policy'] = [dns_query('example.com', q) for q in ('AAAA', 'HTTPS', 'SVCB')]
            result['dns_failure_count'] = sum(not x['ok'] or not x['answers'] for x in result['dns'])
            if result['dns_failure_count']:
                print(f"{backend}: DNS A {result['dns_failure_count']}/9 次失败，保留失败及超时时间。", flush=True)
            if any(not any(x['ok'] and x['answers'] for x in result['dns'][i:i+3]) for i in (0,3,6)):
                raise RuntimeError('某域名的三次 DNS A 查询全部失败。')
            if not all(x['ok'] and x['answers'] == 0 for x in result['ipv4_policy']):
                raise RuntimeError('IPv4 DNS 策略验证失败。')
            if backend == 'sing-box':
                result['hardcoded_dns'] = dns_query('example.com', server='8.8.8.8')
                if not result['hardcoded_dns']['ok']:
                    print('硬编码 DNS 劫持失败；保留功能失败，继续吞吐测试。', flush=True)
            result['serial'] = [download(directory, f'{number}-serial-{i}') for i in range(2)]
            begin = time.monotonic()
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                result['parallel'] = list(pool.map(lambda i: download(directory, f'{number}-parallel-{i}'), range(2)))
            result['parallel_seconds'] = time.monotonic()-begin
            result['parallel_Mbps'] = sum(x.get('bytes',0) for x in result['parallel'])*8/result['parallel_seconds']/1e6
            result['transfer_failure_count'] = sum(not x['ok'] for x in result['serial']+result['parallel'])
            result['measurement_complete'] = True
            result['ok'] = result['transfer_failure_count'] == 0 and result['dns_failure_count'] == 0 and result.get('hardcoded_dns', {'ok':True})['ok']
            if result['transfer_failure_count']:
                result['error'] = '下载存在失败；结果保留，继续其他阶段。'
                print(f"{backend}: {result['transfer_failure_count']}/4 次下载失败。", flush=True)
        except (Exception, KeyboardInterrupt) as error:
            result['ok'] = False
            result['error'] = str(error)
        finally:
            stop(process)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--preflight', action='store_true')
    parser.add_argument('--resume', type=Path, help='resume an existing build/tun-compare-* directory')
    options = parser.parse_args()
    ip = preflight()
    if options.preflight:
        print('预检通过；接下来需要 sudo，用于两种 TUN 的启动和恢复。')
        return
    stamp = datetime.datetime.now().astimezone().strftime('%Y%m%d-%H%M%S')
    directory = ROOT/'build'/('tun-compare-'+stamp)
    previous = None
    if options.resume:
        directory = options.resume.resolve()
        if directory.parent != ROOT/'build' or not directory.name.startswith('tun-compare-'):
            raise RuntimeError('--resume 必须指向本项目 build/tun-compare-* 目录。')
        previous = json.loads((directory/'comparison.json').read_text())
        if previous.get('endpoint') != ENDPOINT or previous.get('ice_transport') != 'tcp' or previous.get('complete'):
            raise RuntimeError('仅可续跑同一 VPS、ICE-TCP 的未完成比较。')
        if previous.get('transport_parameters', dict.fromkeys(PERFORMANCE, 0)) != PERFORMANCE:
            raise RuntimeError('续跑不能改变传输参数；请为新参数开始独立测试。')
        (directory/f'comparison-before-resume-{stamp}.json').write_text(json.dumps(previous, ensure_ascii=False, indent=2)+'\n')
    else:
        directory.mkdir(mode=0o700)
    initial_dns = run(['/usr/sbin/networksetup', '-getdnsservers', SERVICE]).stdout
    initial_route = route(ip)
    keepalive_stop = threading.Event()
    def keepalive():
        while not keepalive_stop.wait(30):
            run(['sudo', '-n', '-v'], check=False)
    keepalive_thread = threading.Thread(target=keepalive, daemon=True)
    keepalive_thread.start()
    result = {'endpoint': ENDPOINT, 'ice_transport': 'tcp', 'order': 'ABBA', 'phases': [],
              'transport_parameters': PERFORMANCE,
              'notes': 'Full TUN, no explicit proxy, 4MiB generated Cloudflare responses; includes public CDN variation.'}
    if previous:
        result = previous
        result['resumed_at'] = stamp
        phases = result['phases']
        keep = 0
        for phase_result in phases:
            # Older reports stopped on a failed download after all measurements.
            # Keep that completed measurement, including the failure, on resume.
            if not phase_result.get('ok') and not ('serial' in phase_result and 'parallel' in phase_result):
                break
            keep += 1
        result.setdefault('prior_attempts', []).extend(phases[keep:])
        result['phases'] = phases[:keep]
    output = directory/'comparison.json'
    def save():
        output.write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
    print(f'结果目录：{directory}；自动运行 ABBA，结束后恢复网络。', flush=True)
    try:
        for i, backend in enumerate(('tun2socks', 'sing-box', 'sing-box', 'tun2socks'), 1):
            if i <= len(result['phases']):
                continue
            value = phase(directory, i, backend, ip)
            result['phases'].append(value)
            result['dns_restored'] = run(['/usr/sbin/networksetup', '-getdnsservers', SERVICE]).stdout == initial_dns
            now = route(ip)
            result['transport_route_restored'] = all(now.get(k)==initial_route.get(k) for k in ('interface', 'gateway'))
            save()
            if not value.get('measurement_complete') or not result['dns_restored'] or not result['transport_route_restored']:
                if not value['ok']:
                    print(value.get('error', '未知阶段错误'), file=sys.stderr, flush=True)
                raise RuntimeError('阶段失败或恢复不完整；停止后续测试。')
        result['summary'] = {}
        for backend in ('tun2socks', 'sing-box'):
            phases = [p for p in result['phases'] if p['backend']==backend]
            serial = [x['Mbps'] for p in phases for x in p['serial'] if x['ok']]
            parallel = [p['parallel_Mbps'] for p in phases if all(x['ok'] for x in p['parallel'])]
            result['summary'][backend] = {
                'serial_success_median_Mbps': statistics.median(serial) if serial else None,
                'parallel_success_mean_Mbps': statistics.mean(parallel) if parallel else None,
                'download_failure_count': sum(not x['ok'] for p in phases for x in p['serial']+p['parallel']),
                'download_attempt_count': sum(len(p['serial']+p['parallel']) for p in phases),
                'dns_median_ms': 1000*statistics.median(x['seconds'] for p in phases for x in p['dns']),
                'dns_first_median_ms': 1000*statistics.median(x['seconds'] for p in phases for x in p['dns'][::3]),
                'dns_repeat_median_ms': 1000*statistics.median(x['seconds'] for p in phases for j,x in enumerate(p['dns']) if j % 3),
                'dns_failure_count': sum(not x['ok'] or not x['answers'] for p in phases for x in p['dns']),
                'dns_query_count': sum(len(p['dns']) for p in phases),
                'startup_mean_seconds': statistics.mean(p['startup_seconds'] for p in phases)}
        result['complete'] = True
        result['all_checks_passed'] = all(p['ok'] for p in result['phases'])
        save()
        print(json.dumps(result['summary'], ensure_ascii=False, indent=2), flush=True)
    finally:
        keepalive_stop.set()
        keepalive_thread.join(timeout=2)
        save()
        print(f'结果：{output}', flush=True)


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print(f'停止：{error}', file=sys.stderr)
        sys.exit(1)
