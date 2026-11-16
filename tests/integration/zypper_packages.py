#!/usr/bin/env python3
"""zypper_package and zypper_repository through the public CLI on an openSUSE Leap node; checked with rpm, zypper, systemctl, and the test repository's request log, never through MMX.

The suite builds a signed test repository on the guest (a handful of small rpm packages, served by a web server that the suite starts) and tests both modules with it. It also installs one real package from openSUSE's own repository. The node's packages stay as they were, except for the tools the fixture needs; the suite removes the packages, the repository, and the key that it added."""
import json
import shlex
import shutil
import time

import vm

LOCK_FILE = '/run/zypp.pid'
REPOSITORY_DIRECTORY = '/srv/mmxtest_repository'
REPOSITORY_ADDRESS = 'http://127.0.0.1:8099/'
BUILD_TOOLS = ['rpm-build', 'createrepo_c']
# The signing key of an OBS repository, an HTTPS address that openSUSE
# serves: it tests signing_key_url with its checksum.
KEY_ADDRESS = 'https://download.opensuse.org/repositories/devel:/languages:/python/openSUSE_Tumbleweed/repodata/repomd.xml.key'

SPEC = '''Name: {name}
Version: {version}
Release: 1
Summary: MMX test package
License: MIT
BuildArch: noarch
{header}
%description
MMX test package.
%install
mkdir -p %{{buildroot}}/usr/share/{name}
echo {version} > %{{buildroot}}/usr/share/{name}/{file}
{install}
%files
/usr/share/{name}/{file}
{files}
{scripts}
'''

# name, version, extra header lines, the file under /usr/share/NAME or a
# shared directory, extra install lines, extra %files lines, scripts.
PACKAGES = [
    ('mmxtest-hello', '1.0', '', 'version', '', '', ''),
    ('mmxtest-app', '1.0', 'Requires: mmxtest-hello', 'version', '', '', ''),
    ('mmxtest-service', '1.0', '', 'version',
     'mkdir -p %{buildroot}/usr/lib/systemd/system\n'
     'printf "[Service]\\nExecStart=/usr/bin/sleep infinity\\n" > %{buildroot}/usr/lib/systemd/system/mmxtest-service.service',
     '/usr/lib/systemd/system/mmxtest-service.service', ''),
    ('mmxtest-provider', '1.0', 'Provides: mmxtest-capability', 'version', '', '', ''),
    ('mmxtest-conflicts', '1.0', 'Conflicts: mmxtest-clasha', 'version', '', '', ''),
    ('mmxtest-badscript', '1.0', '', 'version', '', '', '%post\nexit 1'),
    ('mmxtest-slow', '1.0', '', 'version', '', '', ''),
    ('mmxtest-stuck', '1.0', '', 'version', '', '', ''),
    # zypper takes a newer version only from the vendor of the installed one.
    ('mmxtest-vendor', '1.0', 'Vendor: Vendor A', 'version', '', '', ''),
]
# Two packages that own the same file, and a package without a signature.
CLASH = [('mmxtest-clasha', '1.0'), ('mmxtest-clashb', '1.0')]

# The web server of the test repository. A request for the package
# mmxtest-slow takes 8 seconds, and for mmxtest-stuck 75 seconds, so that the
# zypper that downloads the package holds zypper's lock for that long. Every
# request is logged to the journal.
SERVER = '''import http.server, time
class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        for name, seconds in (("mmxtest-slow", 8), ("mmxtest-stuck", 75)):
            if name in self.path:
                time.sleep(seconds)
        super().do_GET()
http.server.ThreadingHTTPServer(("127.0.0.1", 8099), Handler).serve_forever()
'''


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'zypper-packages'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'suse': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()

    def remote(script, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check, timeout=900).stdout.strip()

    def run(command, expected=None, timeout=900):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=timeout)
        codes = vm.success_codes(command) if expected is None else (expected,)
        assert result.returncode in codes, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='zypper_packages', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def one(module, task, command, expected=None, retry=None):
        entry = {'id': 'task', module: task}
        if retry is not None:
            entry['retry'] = retry
        project([entry])
        return results(run(command, expected))['task']

    def package(task, command, expected=None, retry=None):
        return one('zypper_package', task, command, expected, retry)

    def entries(*names, version='newest_when_first_installed'):
        return [{'name': name, 'version': version} for name in names]

    def is_result(result, status, change, reason=None, tries=None):
        assert result['status'] == status and result['change'] == change and result.get('module_kind') == 'official_extra', result
        if reason is not None:
            assert reason in result.get('reason', ''), result
        if tries is not None:
            assert len(result.get('tries') or [result]) == tries, result
        return result

    def refused(task, reason):
        """The task cannot succeed: it fails in a dryrun and in a run with the same reason, after one try, and changes no package."""
        for command in ('dryrun', 'run'):
            before = installed()
            result = is_result(package(task, command, 1), 'failed', 'unchanged', reason=reason, tries=1)
            assert result['failure_kind'] == 'config' and installed() == before, result

    def mark(message):
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def installed():
        """The installed packages and their versions, from rpm; several versions of a name are joined as the module joins them."""
        found = {}
        for line in remote("rpm -qa --queryformat '%{NAME}\\t%{EVR}\\n'").splitlines():
            name, version = line.split('\t')
            found.setdefault(name, []).append(version)
        return {name: ', '.join(sorted(set(versions))) for name, versions in found.items()}

    def changes(before, after):
        found = []
        for name in sorted(set(before) | set(after)):
            if before.get(name) != after.get(name):
                found.append({'name': name, **({'from': before[name]} if name in before else {}), **({'to': after[name]} if name in after else {})})
        return found

    def repository_requests():
        """How many requests the test repository's web server has logged."""
        return int(remote('journalctl -u mmxtest-repository --no-pager -o cat | grep -c "GET " || true'))

    def main_pid(service):
        return remote(f'systemctl show -p MainPID --value {service}')

    def publish(name, version, vendor=''):
        """Build one more version of a package into the test repository and sign the metadata again."""
        remote(f'/usr/local/bin/build_package {name} {version} {shlex.quote(vendor)}\n/usr/local/bin/rebuild_repository')
        run_refresh()

    def run_refresh():
        remote('zypper --non-interactive refresh mmxtest')

    assert remote('command -v zypper') == '/usr/bin/zypper'
    packages_before = set(installed())
    try:
        # The fixture: the build tools come from the node task that the page
        # shows, then the test repository is built and served.
        specs = {name: SPEC.format(name=name, version=version, header=header, file=file, install=install, files=files, scripts=scripts)
                 for name, version, header, file, install, files, scripts in PACKAGES}
        for name, version in CLASH:
            # Both packages own one file with different content.
            specs[name] = SPEC.format(name=name, version=version, header='', file='f', install='', files='', scripts='').replace(
                f'/usr/share/{name}', '/usr/share/mmxtest-clash').replace(f'echo {version} >', f'echo {name} >')
        script = ['mkdir -p /tmp/mmxtest/specs /tmp/mmxtest/top ' + REPOSITORY_DIRECTORY]
        for name, text in specs.items():
            script.append(f"cat > /tmp/mmxtest/specs/{name}.spec <<'EOF'\n{text}EOF")
        script.append(f"cat > /tmp/mmxtest/server.py <<'EOF'\n{SERVER}EOF")
        is_result(package({'packages': entries(*BUILD_TOOLS)}, 'run'), 'succeeded', 'changed')
        remote('\n'.join(script))
        remote(f'''export GNUPGHOME=/tmp/mmxtest/gnupg; mkdir -p -m700 $GNUPGHOME
gpg --batch --passphrase '' --quick-gen-key 'MMX test repository <test@example.invalid>' rsa2048 sign never 2>/dev/null
gpg --armor --export 'MMX test repository' > /tmp/mmxtest/key.asc
for k in other third; do gpg --batch --passphrase '' --quick-gen-key "MMX $k key <$k@example.invalid>" rsa2048 sign never 2>/dev/null; gpg --armor --export "MMX $k key" > /tmp/mmxtest/${{k}}_key.asc; done
cat > /usr/local/bin/build_package <<'EOS'
#!/bin/sh
# build_package NAME VERSION [VENDOR]: build, sign, and put the package into the repository.
set -eu
sed "s/^Version: .*/Version: $2/; s/echo [0-9.]* >/echo $2 >/; s/^Vendor: .*/Vendor: ${{3:-Vendor A}}/" /tmp/mmxtest/specs/$1.spec > /tmp/mmxtest/specs/$1-$2.spec
rpmbuild -bb --define "_topdir /tmp/mmxtest/top" /tmp/mmxtest/specs/$1-$2.spec >/dev/null 2>&1
cp /tmp/mmxtest/top/RPMS/noarch/$1-$2-1.noarch.rpm {REPOSITORY_DIRECTORY}/
GNUPGHOME=/tmp/mmxtest/gnupg
export GNUPGHOME
if [ "$1" != mmxtest-unsigned ]; then
  rpmsign --define '_gpg_name MMX test repository' --define '_gpg_path /tmp/mmxtest/gnupg' --addsign {REPOSITORY_DIRECTORY}/$1-$2-1.noarch.rpm >/dev/null 2>&1
fi
EOS
cat > /usr/local/bin/rebuild_repository <<'EOS'
#!/bin/sh
set -eu
export GNUPGHOME=/tmp/mmxtest/gnupg
cd {REPOSITORY_DIRECTORY}
rm -rf repodata
createrepo_c . >/dev/null
gpg --batch --armor --detach-sign repodata/repomd.xml
EOS
chmod +x /usr/local/bin/build_package /usr/local/bin/rebuild_repository
for name in {' '.join(name for name, *_ in PACKAGES)} {' '.join(name for name, _ in CLASH)}; do /usr/local/bin/build_package $name 1.0; done
# a package that is not signed
cp /tmp/mmxtest/specs/mmxtest-hello.spec /tmp/mmxtest/specs/mmxtest-unsigned.spec
sed -i 's/^Name: .*/Name: mmxtest-unsigned/; s#/usr/share/mmxtest-hello#/usr/share/mmxtest-unsigned#g' /tmp/mmxtest/specs/mmxtest-unsigned.spec
/usr/local/bin/build_package mmxtest-unsigned 1.0
/usr/local/bin/rebuild_repository
systemd-run --unit mmxtest-repository -p WorkingDirectory={REPOSITORY_DIRECTORY} python3 /tmp/mmxtest/server.py >/dev/null
sleep 1
curl -sf {REPOSITORY_ADDRESS}repodata/repomd.xml >/dev/null''')
        key = remote('cat /tmp/mmxtest/key.asc') + '\n'
        repository = {'name': 'mmxtest', 'url': REPOSITORY_ADDRESS, 'repository_metadata_is_signed': True, 'signing_key_text': key}

        # A real package from openSUSE's own repository: the dryrun names what
        # zypper would install and changes nothing, the run installs it, and
        # the result is what rpm shows changed.
        before = installed()
        preview = is_result(package({'packages': entries('tree')}, 'dryrun'), 'succeeded', 'would_change', reason='would install tree ')
        assert preview['module_outputs']['changed_packages'] == [{'name': 'tree', 'to': preview['module_outputs']['changed_packages'][0]['to']}], preview
        assert installed() == before
        applied = is_result(package({'packages': entries('tree')}, 'run'), 'succeeded', 'changed', reason='installed tree ')
        after = installed()
        assert applied['module_outputs']['changed_packages'] == changes(before, after) and 'tree' in after, (applied, changes(before, after))
        print(f"zypper {remote('zypper --version').split()[1]}; tree {after['tree']}", flush=True)
        mark('a dryrun names what zypper would install from the openSUSE repository and changes nothing; a run installs tree and reports exactly what rpm shows changed')
        for command in ('run', 'dryrun'):
            again = is_result(package({'packages': entries('tree')}, command, 0), 'succeeded', 'unchanged')
            assert again['module_outputs']['changed_packages'] == [] and 'reason' not in again, again
        assert installed() == after
        # rpm and zypper read tree-VERSION and tree.ARCHITECTURE as the package
        # tree, so a removal by such a name would report unchanged while tree
        # stays: it is refused.
        architecture = remote("rpm -q --queryformat '%{ARCH}' tree")
        for other_name in (f"tree-{after['tree']}", f'tree.{architecture}'):
            refused({'packages': [{'name': other_name}], 'state': 'absent'}, f'zypper_package.packages: {other_name} is a name that another package provides, or a package')
            assert 'tree' in installed()
        is_result(package({'packages': [{'name': 'tree'}], 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed tree ')
        mark('a rerun and a dryrun of an installed package are unchanged; a removal by tree-VERSION or tree.ARCHITECTURE, which zypper reads as tree, is refused; the removal removes it')

        # The repository module: key, repository file, import into rpm,
        # unchanged rerun, dryrun, and a package from it.
        preview = is_result(one('zypper_repository', repository, 'dryrun'), 'succeeded', 'would_change', reason='would add the repository mmxtest')
        assert not remote('ls /etc/zypp/repos.d/mmxtest.repo 2>/dev/null || true') and preview['module_outputs']['created'] is True, preview
        keys_before = remote('rpmkeys --list')
        added = is_result(one('zypper_repository', repository, 'run'), 'succeeded', 'changed', reason='added the repository mmxtest at ' + REPOSITORY_ADDRESS)
        assert 'imported the signing key' in added['reason'] and added['module_outputs']['created'] is True, added
        assert 'MMX test repository' in remote('rpmkeys --list') and 'MMX test repository' not in keys_before
        text = remote('cat /etc/zypp/repos.d/mmxtest.repo')
        for line in ('baseurl=' + REPOSITORY_ADDRESS, 'gpgcheck=1', 'repo_gpgcheck=1', 'pkg_gpgcheck=1', 'autorefresh=1', 'enabled=1'):
            assert line in text.splitlines(), text
        for command in ('run', 'dryrun'):
            again = is_result(one('zypper_repository', repository, command, 0), 'succeeded', 'unchanged')
            assert again['module_outputs']['created'] is False, again
        mark('zypper_repository: a dryrun names the repository and the key, changes nothing; a run imports the key into rpm and writes the repository file with every setting spelled out; reruns are unchanged')

        # A package from the new repository: the package task refreshes the
        # repositories, because it has work, and finds the package.
        requests = repository_requests()
        before = installed()
        applied = is_result(package({'packages': entries('mmxtest-app')}, 'run'), 'succeeded', 'changed', reason='installed mmxtest-app 1.0-1, mmxtest-hello 1.0-1')
        assert repository_requests() > requests and applied['module_outputs']['changed_packages'] == changes(before, installed())
        requests = repository_requests()
        for command in ('run', 'dryrun'):
            is_result(package({'packages': entries('mmxtest-app', 'mmxtest-hello')}, command, 0), 'succeeded', 'unchanged')
        assert repository_requests() == requests, 'an unchanged task contacted the repository'
        mark('a package task after the repository task refreshes the repositories and installs the package and its dependency from it; an unchanged task contacts no repository')

        # Removal: zypper removes dependents by itself, so the task previews.
        refused({'packages': [{'name': 'mmxtest-hello'}], 'state': 'absent'},
                'zypper_package: removing mmxtest-hello would also remove mmxtest-app, which depend on them; '
                'to remove these packages too, add also_remove_packages_that_depend_on_these: true')
        removal = {'packages': [{'name': 'mmxtest-hello'}], 'state': 'absent', 'also_remove_packages_that_depend_on_these': True}
        before = installed()
        previewed = is_result(package(removal, 'dryrun'), 'succeeded', 'would_change')
        assert previewed['reason'] == f"would remove mmxtest-app {before['mmxtest-app']}, mmxtest-hello {before['mmxtest-hello']}", previewed
        assert installed() == before
        removed = is_result(package(removal, 'run'), 'succeeded', 'changed')
        assert not {'mmxtest-app', 'mmxtest-hello'} & set(installed()) and removed['module_outputs']['changed_packages'] == changes(before, installed())
        for command in ('run', 'dryrun'):
            is_result(package(removal, command, 0), 'succeeded', 'unchanged')
        mark('removing a package that another needs fails in a dryrun and a run and changes nothing; with also_remove_packages_that_depend_on_these both go; then the removal is unchanged')

        # Names that the task cannot install or remove.
        is_result(package({'packages': entries('mmxtest-no-such-package')}, 'dryrun', 3), 'not_checked', 'unknown',
                  reason='zypper_package: the repositories the node has now offer no mmxtest-no-such-package')
        failed = is_result(package({'packages': entries('mmxtest-no-such-package')}, 'run', 1), 'failed', 'unchanged',
                           reason='zypper_package.packages: the repositories offer no mmxtest-no-such-package', tries=1)
        assert failed['failure_kind'] == 'config', failed
        is_result(package({'packages': entries('mmxtest-provider')}, 'run'), 'succeeded', 'changed')
        not_its_own = 'is a name that another package provides, or a package\'s name with a version or an architecture, not a package of its own; name each package itself'
        refused({'packages': [{'name': 'mmxtest-capability'}], 'state': 'absent'}, 'zypper_package.packages: mmxtest-capability ' + not_its_own)
        refused({'packages': entries('mmxtest-capability')}, 'zypper_package.packages: mmxtest-capability ' + not_its_own)
        for name in ('php*', '/usr/bin/tree', 'libz.so.1()(64bit)', 'mmxtest-hello=1.0', 'mmxtest:mmxtest-hello'):
            refused({'packages': entries(name)}, f'zypper_package.packages: {name} may hold only')
            refused({'packages': [{'name': name}], 'state': 'absent'}, f'zypper_package.packages: {name} may hold only')
        assert 'mmxtest-provider' in installed()
        is_result(package({'packages': [{'name': 'mmxtest-provider'}], 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed mmxtest-provider ')
        mark('a name that no repository has is not checked in a dryrun and fails a run once; a capability that an installed package provides is refused to install and to remove; '
             'a pattern, a path, a capability with parentheses, a version, and a repository in front of the name are refused, to install and to remove')

        # What the dry run does not see: the files two packages share, and the
        # signature of a package. The run finds them, and changes no package.
        for names, reason in ((['mmxtest-clasha', 'mmxtest-clashb'], 'is in both mmxtest-clasha and mmxtest-clashb, with different content'), (['mmxtest-unsigned'], 'Signature verification failed')):
            before = installed()
            is_result(package({'packages': entries(*names)}, 'dryrun'), 'succeeded', 'would_change', reason='would install ' + names[0])
            project([{'id': 'task', 'retry': False, 'zypper_package': {'packages': entries(*names)}}])
            failure = is_result(results(run('run', 1))['task'], 'failed', 'changed', reason='zypper_package: installing ' + ', '.join(names))
            assert reason in failure['reason'] and installed() == before, failure
        # A conflict that a package declares is in the dry run: the solver
        # finds it, and the dryrun fails as a run does.
        is_result(package({'packages': entries('mmxtest-hello')}, 'run'), 'succeeded', 'changed')
        mark('a dryrun does not see two packages that own one file, or a package without a signature: it says it would install; the run fails with zypper\'s message, reports the task changed, and installs nothing')

        # A script of a package that fails: the package is installed, the
        # task fails once, and the next run is unchanged.
        project([{'id': 'task', 'zypper_package': {'packages': entries('mmxtest-badscript')}}])
        failure = is_result(results(run('run', 1))['task'], 'failed', 'changed', reason='a script of a package failed (exit code 107)', tries=1)
        assert 'mmxtest-badscript' in installed(), failure
        is_result(package({'packages': entries('mmxtest-badscript')}, 'run', 0), 'succeeded', 'unchanged')
        is_result(package({'packages': [{'name': 'mmxtest-badscript'}], 'state': 'absent'}, 'run'), 'succeeded', 'changed')
        mark('a package whose script fails is installed, the task fails after one try and names the script, and the next run is unchanged')

        # Version rules.
        is_result(package({'packages': [{'name': 'mmxtest-hello', 'version': '1.0'}]}, 'run', 0), 'succeeded', 'unchanged')
        publish('mmxtest-hello', '2.0')
        is_result(package({'packages': entries('mmxtest-hello')}, 'run', 0), 'succeeded', 'unchanged')
        newest = package({'packages': entries('mmxtest-hello', version='newest_on_every_run')}, 'dryrun')
        is_result(newest, 'succeeded', 'would_change', reason='would upgrade mmxtest-hello from 1.0-1 to 2.0-1')
        assert installed()['mmxtest-hello'] == '1.0-1'
        exact = {'packages': [{'name': 'mmxtest-hello', 'version': '2.0'}]}
        is_result(package(exact, 'run'), 'succeeded', 'changed', reason='upgraded mmxtest-hello from 1.0-1 to 2.0-1')
        assert installed()['mmxtest-hello'] == '2.0-1'
        is_result(package(exact, 'run', 0), 'succeeded', 'unchanged')
        is_result(package({'packages': [{'name': 'mmxtest-hello', 'version': '2.0-1'}]}, 'run', 0), 'succeeded', 'unchanged')
        refused({'packages': [{'name': 'mmxtest-hello', 'version': '1.0'}]},
                'zypper_package.packages: mmxtest-hello is installed at 2.0-1, a version that zypper does not replace by the version asked for; this module never downgrades')
        refused({'packages': [{'name': 'mmxtest-hello', 'version': '>=1.0'}]}, 'zypper_package.packages: mmxtest-hello: version may hold only')
        is_result(package({'packages': [{'name': 'mmxtest-hello', 'version': '9.9'}]}, 'run', 1), 'failed', 'unchanged',
                  reason='zypper_package.packages: the repositories offer no mmxtest-hello=9.9')
        mark('an exact version is installed or updated to and then unchanged, with or without its release; newest_on_every_run updates; an older version than the installed one, '
             'an operator, and a version that no repository has are refused')

        # zypper takes a newer version only from the vendor of the installed one,
        # and says so; the task says so too.
        is_result(package({'packages': entries('mmxtest-vendor', version='newest_on_every_run')}, 'run'), 'succeeded', 'changed', reason='installed mmxtest-vendor 1.0-1')
        publish('mmxtest-vendor', '2.0', 'Vendor B')
        for command in ('dryrun', 'run'):
            kept = is_result(package({'packages': entries('mmxtest-vendor', version='newest_on_every_run')}, command, 0), 'succeeded', 'unchanged',
                             reason="not updated: There is an update candidate for 'mmxtest-vendor' from vendor 'Vendor B', while the current vendor is 'Vendor A'.")
            assert installed()['mmxtest-vendor'] == '1.0-1', kept
        mark('a newer version from another vendor is not installed: newest_on_every_run is unchanged, and its reason quotes zypper\'s remark about the vendors')

        # A node task block restarts the service when the package changes.
        block = {'task_block': {'id': 'service', 'changes': [{'id': 'service_package', 'zypper_package': {'packages': entries('mmxtest-service', version='newest_on_every_run')}}],
                                'activate': [{'id': 'service_unit', 'systemd_service': {'name': 'mmxtest-service', 'state': 'running'}}]}}
        project([block])
        run('run')
        assert remote('systemctl is-active mmxtest-service') == 'active' and installed()['mmxtest-service'] == '1.0-1'
        pid = main_pid('mmxtest-service')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and main_pid('mmxtest-service') == pid
        mark('in a node task block, the service starts once and an unchanged package restarts nothing')

        # A newer package: a dryrun predicts the restart; the run restarts once.
        publish('mmxtest-service', '2.0')
        previewed = results(run('dryrun'))
        assert previewed['service_package']['change'] == 'would_change' and previewed['service_unit']['change'] == 'would_change', previewed
        restarted = results(run('run'))
        assert installed()['mmxtest-service'] == '2.0-1' and restarted['service_package']['change'] == 'changed' and restarted['service_unit']['change'] == 'changed', restarted
        assert main_pid('mmxtest-service') not in ('0', pid)
        pid = main_pid('mmxtest-service')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and main_pid('mmxtest-service') == pid
        mark('a newer package version: a dryrun predicts the restart, a run updates the package and restarts the service once, the next run restarts nothing')

        # Whole-system update: the repository has a newer mmxtest-hello.
        publish('mmxtest-hello', '3.0')
        before = installed()
        previewed = is_result(package({'upgrade_all_packages': True}, 'dryrun'), 'succeeded', 'would_change', reason='would upgrade ')
        assert {'name': 'mmxtest-hello', 'from': '2.0-1', 'to': '3.0-1'} in previewed['module_outputs']['changed_packages'], previewed
        assert installed() == before
        is_result(package({'packages': entries('mmxtest-hello')}, 'run', 0), 'succeeded', 'unchanged')
        upgraded = is_result(package({'upgrade_all_packages': True}, 'run'), 'succeeded', 'changed', reason='upgraded ')
        assert upgraded['module_outputs']['changed_packages'] == changes(before, installed()) and installed()['mmxtest-hello'] == '3.0-1', upgraded
        for _ in range(3):
            # A package server may publish a package between two runs.
            again = package({'upgrade_all_packages': True}, 'run')
            if again['change'] == 'unchanged':
                break
        is_result(again, 'succeeded', 'unchanged')
        mark('a dryrun of the whole-system update names the updates and changes nothing; the run updates; an update with nothing newer is unchanged although it refreshed')

        # zypper's lock: the task waits while another zypper holds it.
        remote('systemctl stop mmxtest-lock-holder 2>/dev/null || true')
        remote('systemd-run --unit mmxtest-lock-holder zypper --non-interactive --no-refresh install --download-only -y mmxtest-slow >/dev/null\n'
               'for i in 1 2 3 4 5 6 7 8 9 10; do [ -s ' + LOCK_FILE + ' ] && break; sleep 1; done')
        started = time.monotonic()
        is_result(package({'packages': entries('mmxtest-hello')}, 'run', 0), 'succeeded', 'unchanged')
        is_result(package({'packages': entries('mmxtest-provider')}, 'run'), 'succeeded', 'changed', reason='installed mmxtest-provider ', tries=1)
        waited = time.monotonic() - started
        assert waited >= 3 and 'mmxtest-provider' in installed(), waited
        # A lock that stays.
        remote('systemd-run --unit mmxtest-lock-holder zypper --non-interactive --no-refresh install --download-only -y mmxtest-stuck >/dev/null\n'
               'for i in 1 2 3 4 5 6 7 8 9 10; do [ -s ' + LOCK_FILE + ' ] && break; sleep 1; done')
        project([{'id': 'task', 'retry': False, 'zypper_package': {'packages': [{'name': 'mmxtest-provider'}], 'state': 'absent'}}])
        stale = is_result(results(run('run', 1))['task'], 'failed', 'unchanged', reason='zypper_package: previewing the removal of mmxtest-provider: another zypper still holds its lock ' + LOCK_FILE + ' after 60 seconds')
        assert 'mmxtest-provider' in installed(), stale
        remote('systemctl stop mmxtest-lock-holder; sleep 1')
        is_result(package({'packages': [{'name': 'mmxtest-provider'}], 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed mmxtest-provider ')
        mark(f'a task waits while another zypper holds its lock and installs when it goes ({waited:.0f} s); a lock held for 60 seconds fails the task with zypper\'s message, and after the holder ends the task works')

        # Removal takes out only the keys that the module imported, and only
        # when no other repository's key file holds them.
        distribution_key_file = '/usr/lib/rpm/gnupg/keys/gpg-pubkey-09d9ea69-68595a8c.asc'
        distribution_key = remote(f'cat {distribution_key_file}') + '\n'
        distribution_key_name = 'SUSE 16 Package Signing Key'
        assert distribution_key_name in remote('rpmkeys --list')
        own = dict(repository, name='mmxdistribution', signing_key_text=distribution_key)
        added = is_result(one('zypper_repository', own, 'run'), 'succeeded', 'changed', reason='added the repository mmxdistribution')
        assert 'imported the signing key' not in added['reason'] and remote('cat /etc/zypp/mmx_repository_keys/mmxdistribution.imported_key_ids || true') == '', added
        is_result(one('zypper_repository', {'name': 'mmxdistribution', 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed the repository mmxdistribution')
        assert distribution_key_name in remote('rpmkeys --list')
        remote('zypper --non-interactive refresh --force openSUSE:repo-oss')
        # Two repositories with one key: the first to go leaves the key.
        second = dict(repository, name='mmxsecond')
        is_result(one('zypper_repository', second, 'run'), 'succeeded', 'changed', reason='added the repository mmxsecond')
        is_result(one('zypper_repository', {'name': 'mmxtest', 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed the repository mmxtest')
        assert 'MMX test repository' in remote('rpmkeys --list')
        remote('zypper --non-interactive refresh --force mmxsecond')
        is_result(one('zypper_repository', {'name': 'mmxsecond', 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed the repository mmxsecond; removed the signing key')
        assert 'MMX test repository' not in remote('rpmkeys --list')
        # A new key replaces the old one, which leaves rpm.
        other_key, third_key = remote('cat /tmp/mmxtest/other_key.asc') + '\n', remote('cat /tmp/mmxtest/third_key.asc') + '\n'
        rotating = dict(repository, name='mmxrotating', signing_key_text=other_key)
        is_result(one('zypper_repository', rotating, 'run'), 'succeeded', 'changed', reason='imported the signing key')
        assert 'MMX other key' in remote('rpmkeys --list')
        is_result(one('zypper_repository', dict(rotating, signing_key_text=third_key), 'run'), 'succeeded', 'changed', reason='(the key that the task replaced)')
        listed = remote('rpmkeys --list')
        assert 'MMX other key' not in listed and 'MMX third key' in listed, listed
        is_result(one('zypper_repository', {'name': 'mmxrotating', 'state': 'absent'}, 'run'), 'succeeded', 'changed')
        assert 'MMX third key' not in remote('rpmkeys --list') and distribution_key_name in remote('rpmkeys --list')
        mark('removing a repository removes only the keys the module imported: the distribution\'s own key stays, and zypper refreshes the distribution repository afterwards; of two repositories with one key the last to go removes it; a new key replaces the old one in rpm')

        # The repository again, to show a key that was never imported.
        is_result(one('zypper_repository', repository, 'run'), 'succeeded', 'changed', reason='added the repository mmxtest')
        # A repository whose key was never imported is not trusted silently:
        # zypper refuses it, and the package task fails with zypper's message.
        is_result(one('zypper_repository', {'name': 'mmxtest', 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed the repository mmxtest; removed the signing key')
        assert 'MMX test repository' not in remote('rpmkeys --list') and not remote('ls /etc/zypp/repos.d/mmxtest.repo /etc/zypp/mmx_repository_keys/mmxtest.asc 2>/dev/null || true')
        remote(f'printf "[mmxmanual]\\nname=mmxmanual\\nenabled=1\\nautorefresh=1\\nbaseurl={REPOSITORY_ADDRESS}\\ntype=rpm-md\\ngpgcheck=1\\nrepo_gpgcheck=1\\n" > /etc/zypp/repos.d/mmxmanual.repo')
        project([{'id': 'task', 'retry': False, 'zypper_package': {'packages': entries('mmxtest-slow')}}])
        failure = is_result(results(run('run', 1))['task'], 'failed', 'unchanged', reason='zypper_package: refreshing the repositories')
        assert 'mmxtest-slow' not in installed(), failure
        remote('zypper --non-interactive removerepo mmxmanual')
        mark('removing the repository removes its file, its key file, and its key from rpm; a repository whose key was never imported is refused: the refresh fails with zypper\'s message and nothing is installed')

        # A key from a web address, checked against its SHA-256.
        sha = remote(f"curl -sf {KEY_ADDRESS} | sha256sum | cut -d' ' -f1")
        by_address = {'name': 'mmxobs', 'url': 'https://download.opensuse.org/repositories/devel:/languages:/python/openSUSE_Tumbleweed/', 'repository_metadata_is_signed': True,
                      'signing_key_url': KEY_ADDRESS, 'signing_key_sha256': '0' * 64}
        refused_key = is_result(one('zypper_repository', by_address, 'run', 1), 'failed', 'unchanged', reason='zypper_repository.signing_key_sha256: is not the SHA-256 of the key at signing_key_url')
        assert not remote('ls /etc/zypp/repos.d/mmxobs.repo 2>/dev/null || true'), refused_key
        by_address['signing_key_sha256'] = sha
        is_result(one('zypper_repository', by_address, 'run'), 'succeeded', 'changed', reason='added the repository mmxobs')
        is_result(one('zypper_repository', by_address, 'run', 0), 'succeeded', 'unchanged')
        is_result(one('zypper_repository', {'name': 'mmxobs', 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed the repository mmxobs')
        is_result(one('zypper_repository', {'name': 'mmxobs', 'state': 'absent'}, 'run', 0), 'succeeded', 'unchanged')
        mark('a signing key from a web address must have the SHA-256 in the task; with it the repository is added, a rerun is unchanged, and the removal is unchanged the second time')
    finally:
        remote('systemctl stop mmxtest-lock-holder mmxtest-repository 2>/dev/null || true\n'
               'systemctl reset-failed 2>/dev/null || true\n'
               'for r in mmxtest mmxsecond mmxrotating mmxdistribution mmxmanual mmxobs; do zypper --non-interactive removerepo $r >/dev/null 2>&1 || true; done\n'
               'for k in $(rpm -qa gpg-pubkey --qf "%{NAME}-%{VERSION}-%{RELEASE} %{SUMMARY}\\n" | grep -E "MMX (test|other|third)" | cut -d" " -f1); do rpm -e $k; done\n'
               'rm -rf /srv/mmxtest_repository /tmp/mmxtest /usr/local/bin/build_package /usr/local/bin/rebuild_repository /etc/zypp/mmx_repository_keys')
        added = sorted(set(installed()) - packages_before)
        if added:
            remote('zypper --non-interactive remove -y ' + ' '.join(added))
    assert set(installed()) == packages_before
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} zypper_package and zypper_repository workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
