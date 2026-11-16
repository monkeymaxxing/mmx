#!/usr/bin/env python3
"""Run the fixed restart interface as an ordinary, non-administrator account."""
import json
import shlex
import shutil
import time

import vm

REQUESTS = '/run/mmx-demo-request'
RESULT = '/run/mmx-demo-control/result'


def main():
    vm.up()
    vm.reset()
    node, ssh = vm.connection()
    workspace = vm.ROOT / 'limited-control'
    if workspace.exists():
        shutil.rmtree(workspace)
    shutil.copytree(vm.REPO / 'examples/limited-control', workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def as_requester(script):
        return remote('runuser -u mmx-requester -- sh -ec ' + shlex.quote(script))

    def run(command):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "restart_control"), '--json'], cwd=workspace, check=False)
        assert result.returncode in vm.success_codes(command), (result.stdout, result.stderr)
        return json.loads(result.stdout)

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def finished():
        # The documented reading rule: once the request is gone, the result
        # names the run serving it; wait until that run has a final result.
        deadline = time.monotonic() + 15
        state = ''
        while time.monotonic() < deadline:
            state = as_requester(f'if test -e {REQUESTS}/"$USER"; then echo pending; else cat {RESULT}; fi')
            if state != 'pending' and not state.endswith(' running'):
                return state
            time.sleep(.1)
        raise AssertionError('request did not finish: ' + state)

    def request(expected):
        as_requester(f'touch {REQUESTS}/"$USER"')
        result = finished()
        assert result.endswith(' ' + expected), result
        return result

    def invocation():
        return remote('systemctl show -p InvocationID --value mmx-demo.service')

    def delay_restarts():
        # Keep each restart running for two seconds and count its starts.
        remote("mkdir -p /etc/systemd/system/mmx-demo.service.d; : > /var/tmp/mmx-control-starts; "
               "printf \"[Service]\\nExecStartPre=+/bin/sh -c 'echo start >> /var/tmp/mmx-control-starts; sleep 2'\\n\" > /etc/systemd/system/mmx-demo.service.d/delay.conf; "
               "systemctl daemon-reload; systemctl reset-failed")

    def hold_restarts():
        # Keep each restart running until the test releases it, and count its starts.
        remote("mkdir -p /etc/systemd/system/mmx-demo.service.d; : > /var/tmp/mmx-control-starts; rm -f /var/tmp/mmx-control-release; "
               "printf \"[Service]\\nExecStartPre=+/bin/sh -c 'echo start >> /var/tmp/mmx-control-starts; "
               "while ! test -e /var/tmp/mmx-control-release; do sleep 0.1; done'\\n\" > /etc/systemd/system/mmx-demo.service.d/delay.conf; "
               "systemctl daemon-reload; systemctl reset-failed")

    def starts():
        return len(remote('cat /var/tmp/mmx-control-starts').splitlines())

    def cleanup():
        remote("systemctl disable --now mmx-demo-restart.path mmx-demo.service 2>/dev/null || true; "
               "systemctl stop mmx-demo-restart.service 2>/dev/null || true; "
               "rm -f /etc/systemd/system/mmx-demo-restart.path /etc/systemd/system/mmx-demo-restart.service "
               "/etc/systemd/system/mmx-demo.service /etc/tmpfiles.d/mmx-demo-control.conf /usr/local/libexec/mmx-demo-result; "
               f"rm -rf /run/mmx-demo-control {REQUESTS} /etc/systemd/system/mmx-demo.service.d; rm -f /var/tmp/mmx-control-starts /var/tmp/mmx-control-release; systemctl daemon-reload; "
               "systemctl reset-failed; userdel mmx-requester 2>/dev/null || true; userdel mmx-demo 2>/dev/null || true; "
               "groupdel mmx-operators 2>/dev/null || true; groupdel mmx-demo 2>/dev/null || true")

    cleanup()
    try:
        run('validate')
        run('dryrun')
        remote(f'test ! -e /run/mmx-demo-control; test ! -e {REQUESTS}; test ! -e /etc/systemd/system/mmx-demo.service')
        run('run')
        before = invocation()
        assert before
        result = run('run')
        assert result['entries'][0]['response']['change'] == 'unchanged', result
        assert invocation() == before
        remote('systemd-analyze verify /etc/systemd/system/mmx-demo-restart.path /etc/systemd/system/mmx-demo-restart.service')
        mark('example previews without mutation, installs valid units, and reruns unchanged without restarting the app')

        remote('useradd --system -M -s /bin/sh -G mmx-operators mmx-requester')
        # The recipe must correct existing ownership and runtime metadata, not
        # just rely on root owning newly created files or on the next boot.
        remote('chown mmx-requester /etc/systemd/system/mmx-demo-restart.service '
               '/etc/systemd/system/mmx-demo-restart.path '
               f'/etc/tmpfiles.d/mmx-demo-control.conf /usr/local/libexec/mmx-demo-result {RESULT} {REQUESTS}; '
               f'chmod 0777 /run/mmx-demo-control {REQUESTS}')
        run('run')
        assert invocation() == before
        assert remote(f'stat -c "%A %U %G" /run/mmx-demo-control {RESULT} {REQUESTS}').splitlines() == [
            'drwxr-xr-x root root', '-rw-r--r-- root root', 'drwxrwx--T root mmx-operators']
        result = run('run')
        assert result['entries'][0]['response']['change'] == 'unchanged', result
        as_requester(f'test -w {REQUESTS}; '
                     f'test ! -w /run/mmx-demo-control; test ! -w {RESULT}; '
                     'test ! -w /etc/systemd/system/mmx-demo-restart.service; '
                     'test ! -w /etc/systemd/system/mmx-demo-restart.path; '
                     'test ! -w /etc/tmpfiles.d/mmx-demo-control.conf; '
                     'test ! -w /usr/local/libexec/mmx-demo-result; '
                     '! sudo -n true 2>/dev/null')
        remote(f'! runuser -u nobody -- touch {REQUESTS}/nobody 2>/dev/null')
        request('success')
        assert invocation() != before
        before = invocation()
        as_requester(f'printf "touch /run/mmx-unwanted-command" > {REQUESTS}/"$USER"')
        finished()
        assert invocation() != before
        remote('test ! -e /run/mmx-unwanted-command')
        # Without the watcher consuming requests, show that a member cannot
        # remove another account's request.
        remote(f'systemctl stop mmx-demo-restart.path; touch {REQUESTS}/root-request')
        as_requester(f'! rm -f {REQUESTS}/root-request 2>/dev/null')
        remote(f'test -e {REQUESTS}/root-request; find {REQUESTS} -mindepth 1 -delete; systemctl start mmx-demo-restart.path')
        request('success')
        mark('group member requests the fixed restart and reads its result, without sudo '
             'or control over commands and units')

        remote("mkdir -p /etc/systemd/system/mmx-demo.service.d; "
               "printf '[Service]\\nExecStart=\\nExecStart=/bin/false\\nType=oneshot\\n' > /etc/systemd/system/mmx-demo.service.d/fail.conf; "
               "systemctl daemon-reload; systemctl reset-failed mmx-demo-restart.service")
        request('exit-code')
        remote('rm /etc/systemd/system/mmx-demo.service.d/fail.conf; systemctl daemon-reload; systemctl reset-failed')
        request('success')
        mark('failed restart is visible in the readable result and the next request succeeds after the app is fixed')

        hold_restarts()
        as_requester(f'touch {REQUESTS}/"$USER"')
        deadline = time.monotonic() + 15
        while starts() == 0 and time.monotonic() < deadline:
            time.sleep(.1)
        first = remote(f'cat {RESULT}')
        assert first.endswith(' running'), first
        # The held first run still owns the result, so this request arrives while it runs.
        assert as_requester(f'touch {REQUESTS}/"$USER"; cat {RESULT}') == first
        remote('touch /var/tmp/mmx-control-release')
        result = finished()
        assert result.split()[0] != first.split()[0] and result.endswith(' success'), (first, result)
        assert starts() == 2
        remote('rm -f /etc/systemd/system/mmx-demo.service.d/delay.conf /var/tmp/mmx-control-release; systemctl daemon-reload; systemctl reset-failed')
        mark('a request made while the restart runs is kept and served by one more restart after it')

        # Keep the action running so a burst can only coalesce, never run concurrently.
        delay_restarts()
        as_requester(f'for n in 1 2 3 4 5; do touch {REQUESTS}/"$USER"; done')
        finished()
        assert 1 <= starts() <= 2
        assert remote('systemctl show -p ActiveState --value mmx-demo.service') == 'active'
        remote('rm /etc/systemd/system/mmx-demo.service.d/delay.conf; systemctl daemon-reload; systemctl reset-failed')
        mark('burst requests coalesce through systemd instead of creating concurrent restart workers')

        remote('systemctl stop mmx-demo-restart.path mmx-demo-restart.service; '
               'systemctl reset-failed; systemctl start mmx-demo-restart.path')
        for _ in range(5):
            request('success')
        as_requester(f'touch {REQUESTS}/"$USER"')
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if remote('systemctl show -p ActiveState --value mmx-demo-restart.path') == 'failed':
                break
            time.sleep(.1)
        assert remote('systemctl show -p ActiveState --value mmx-demo-restart.path') == 'failed'
        remote('systemctl reset-failed mmx-demo.service mmx-demo-restart.service mmx-demo-restart.path; '
               'systemctl start mmx-demo-restart.path')
        request('success')
        mark('native activation limit stops a request burst; documented administrator reset restores the interface')

        vm.reboot()
        node, ssh = vm.connection()
        vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
        assert remote(f'cat {RESULT}') == 'No request since boot'
        assert remote(f'stat -c "%A %U %G" {REQUESTS}') == 'drwxrwx--T root mmx-operators'
        before = invocation()
        result = run('run')
        assert result['entries'][0]['response']['change'] == 'unchanged', result
        assert invocation() == before
        request('success')
        assert invocation() != before
        mark('real reboot recreates the protected request directory before watching; unchanged MMX run and next user request work')
        (workspace / 'results.json').write_text(json.dumps({'passed': passed}, indent=2))
        print(f'{len(passed)} limited-control workflows passed', flush=True)
    finally:
        cleanup()


if __name__ == '__main__':
    vm.run_suite(main)
