#!/usr/bin/env python3
"""docker_image, docker_compose_project, and the Docker forms of wait_for through the public CLI, checked with docker itself and with the containers' own answers."""
import base64
import json
import shlex
import shutil
import time

import image_registry
import vm

REGISTRY = image_registry.ADDRESS
# The suite's own Compose project, and the container that systemd runs.
SHOP = '/opt/mmx-test-shop'
SHOP_PORT = 18080
CONVERTER = 'mmx-test-converter'
CONVERTER_PORT = 18090
# The example deploy-with-docker-compose.
EXAMPLE_SHOP = '/opt/shop'
EXAMPLE_PORT = 8080
HEALTH_CHECK = '["CMD", "/container-test-program", "check-health"]'
# What a dryrun says of each service while an earlier node task of the same
# dryrun would change the project's files.
FILES_WOULD_CHANGE = 'an earlier node task of this dryrun would change a file of this Compose project or of this node task block'
# A run's word for each action that a dryrun predicts.
DONE = {'would_create': 'created', 'would_recreate': 'recreated', 'would_start': 'started', 'would_stop': 'stopped', 'would_remove': 'removed',
        'unchanged': 'unchanged'}
SAMPLES = '/var/tmp/mmx-test-docker'

# Docker's repository for the Red Hat family; Rocky Linux and AlmaLinux take
# the engine from the CentOS directory, which is the only one that has it.
DOCKER_RPM_KEY_SHA256 = 'e6c650e0700b1bf4868b693b30761b926844befc8a0acb7ac0dd9b1faf1b7423'
START_DOCKER = {'systemd_service': {'name': 'docker', 'state': 'running', 'start_at_boot': True}}

# The names of the installed packages, to find those that came with Docker.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null 2>&1; then dpkg-query -W -f '${Package}\\n'; "
                      "else rpm -qa --qf '%{NAME}\\n'; fi | sort -u")
# Docker leaves its bridge, its firewall chains, and its directories when its
# packages go; this removes them, so that the next suite finds the guest as
# this one found it. The firewall rules go before the packages, which may take
# the only tool that shows them. PACKAGES_THE_SUITE_ADDED names the packages
# that came with Docker.
REMOVE_DOCKER = r'''
if command -v docker >/dev/null 2>&1 && systemctl is-active -q docker.service; then
  docker ps --all --quiet | xargs -r docker rm --force --volumes >/dev/null 2>&1 || true
  docker volume ls --quiet | xargs -r docker volume rm --force >/dev/null 2>&1 || true
  docker network prune --force >/dev/null 2>&1 || true
fi
for unit in mmx-test-converter.service docker-image-cleanup.timer docker-image-cleanup.service; do
  systemctl disable --now $unit >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/$unit
done
systemctl disable --now docker.socket docker.service containerd.service >/dev/null 2>&1 || true
systemctl daemon-reload
systemctl reset-failed >/dev/null 2>&1 || true
ip link delete docker0 2>/dev/null || true
for tool in iptables ip6tables; do
  if command -v $tool-save >/dev/null 2>&1 && $tool-save 2>/dev/null | grep -qi docker; then
    $tool-save | grep -vi docker | sed 's/^:FORWARD DROP/:FORWARD ACCEPT/' | $tool-restore
  fi
done
remove_packages() {
  # Ubuntu's own daily package jobs may hold dpkg's lock for a while.
  if command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 purge -y -q "$@" >/dev/null 2>&1
  else dnf remove -y -q "$@" >/dev/null 2>&1
  fi
}
# What this run added goes in one command. What an interrupted run may have
# left goes name by name: a name that the package manager does not know
# stops its whole command.
[ -z "${PACKAGES_THE_SUITE_ADDED:-}" ] || remove_packages $PACKAGES_THE_SUITE_ADDED || true
for package in docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-buildx-plugin docker-ce-rootless-extras \
    docker-model-plugin docker.io docker-cli docker-compose docker-compose-v2; do
  remove_packages $package || true
done
rm -f /etc/apt/sources.list.d/docker.sources /etc/apt/keyrings/docker.asc /etc/apt/preferences.d/mmx-docker-ce.pref
rm -f /etc/yum.repos.d/docker-ce-stable.repo /etc/pki/rpm-gpg/RPM-GPG-KEY-docker-ce-stable
rm -rf /var/lib/docker /var/lib/containerd /etc/docker /run/docker /run/docker.sock /run/containerd /root/.docker
rm -rf /opt/mmx-test-shop /opt/mmx-test-vault /opt/mmx-test-adopted /opt/mmx-test-ordered /opt/mmx-test-twin /opt/mmx-test-platforms /opt/shop /srv/shop-backups /var/tmp/mmx-test-docker
userdel deploy 2>/dev/null || true
groupdel deploy 2>/dev/null || true
groupdel docker 2>/dev/null || true
'''


def main():
    started = time.monotonic()
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    vm.build()
    workspace = vm.ROOT / 'docker-containers'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    mmx = vm.ROOT / 'bin/mmx'
    registry = image_registry.Registry(name=vm.NAME)
    passed, printed = [], []
    vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = image_registry.PASSWORD
    login = {'secrets_from_env': {'registry_password': 'MMX_TEST_REGISTRY_PASSWORD'}}

    def remote(script, input=None):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], input=input).stdout.strip()

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='docker_containers', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **settings)

    def mmx_run(command, arguments, directory, expected, timeout):
        proc = vm.cmd([mmx, command, *arguments, '--json'], cwd=directory, check=False, timeout=timeout)
        printed.append(proc.stdout + proc.stderr)
        assert proc.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, proc.stdout, proc.stderr)
        return json.loads(proc.stdout)

    def run(command, expected=None, timeout=300):
        return mmx_run(command, vm.playbook_args(command), workspace, expected, timeout)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def changes(record):
        return {name: r['change'] for name, r in results(record).items()}

    def unchanged(record):
        """Whether no node task of a run changed anything; the assertion's message names those that did."""
        different = {r['task']: (r['status'], r['change'], r.get('reason'), r.get('value')) for r in record['entries'][0]['response']['results'] if r['change'] != 'unchanged'}
        assert not different, different
        return True

    def changed(record):
        """The full names of the node tasks that changed something."""
        return {r['task'] for r in record['entries'][0]['response']['results'] if r['change'] != 'unchanged'}

    def project_result(record):
        """The results of the docker_compose_project tasks of a run."""
        return [r for r in record['entries'][0]['response']['results'] if 'services' in (r.get('value') or {})]

    def services(result):
        return {s['name']: s['action'] for s in result['value']['services']}

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def docker(arguments):
        try:
            return remote('docker ' + arguments)
        except RuntimeError as error:
            if 'Cannot connect to the Docker daemon' not in str(error):
                raise
            # The daemon ran a moment ago: show what the node's journal says
            # about it, because the guest is gone when the suite has failed.
            journal = remote('systemctl --no-pager --lines=0 status docker.service docker.socket containerd.service || true\n'
                             'journalctl --no-pager --quiet --lines=80 --unit=docker.service --unit=docker.socket --unit=containerd.service || true')
            raise RuntimeError(f'{error}\nthe state and the last journal lines of Docker on the node:\n{journal}') from None

    def image_on_node(reference):
        """The image ID docker itself reports, None when the node has no such image."""
        return remote(f"docker image inspect --format '{{{{.Id}}}}' {reference} 2>/dev/null || true") or None

    def answer(port, was=None):
        """The container's own answer on a published port. With was, wait
        until it comes from another process than that answer's."""
        deadline = time.monotonic() + 30
        while True:
            text = remote(f"python3 -c \"import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:{port}/', timeout=3).read().decode())\" 2>/dev/null || true")
            if text and (was is None or json.loads(text)['process_started_at'] != was['process_started_at']):
                return json.loads(text)
            assert time.monotonic() < deadline, f'no new answer on port {port}: {text!r}'
            time.sleep(0.3)

    def container_ids(directory):
        return remote(f"docker ps --all --no-trunc --filter label=com.docker.compose.project.working_dir={directory} "
                      "--format '{{.Label \"com.docker.compose.service\"}}={{.ID}} {{.State}}' | sort").splitlines()

    def no_image_layer_was_fetched():
        fetched = [r for r in registry.requests() if r['blob_kind'] == 'image_layer']
        assert not fetched, fetched

    def failed_once(result, *texts):
        assert result['status'] == 'failed' and result.get('failure_kind') == 'missing_prerequisite' and len(result.get('tries') or [result]) == 1, result
        for text in texts:
            assert text in result['reason'], result

    remote(REMOVE_DOCKER)
    forwarding = remote('sysctl -n net.ipv4.ip_forward')
    packages_before = set(remote(INSTALLED_PACKAGES).split())
    try:
        registry.start()
        os_id, version_id = remote('. /etc/os-release; echo "$ID $VERSION_ID"').split()
        pinned = registry.publish('mmx-test/pinned', '1', image_version='1')
        moving = registry.publish('mmx-test/moving', 'stable', image_version='1')
        secret = registry.publish('private/app', '1', image_version='1')

        # Before Docker is installed: every task says what is missing and is
        # not tried again; a dryrun cannot check; and a removal finds nothing
        # to remove.
        image = {'id': 'image', 'docker_image': {'image': pinned['reference'], 'pull_image': 'when_missing'}}
        compose = {'id': 'compose', 'docker_compose_project': {'compose_project_name': 'mmx-test-shop', 'compose_project_dir': SHOP, 'state': 'running', 'pull_images': 'when_missing', 'containers_of_removed_services': 'remove'}}
        waiting = {'id': 'waiting', 'wait_for': {'docker_compose_project_is_ready': SHOP, 'wait_at_most': '30s'}}
        removed = {'id': 'removed', 'docker_compose_project': {'compose_project_name': 'mmx-test-shop', 'compose_project_dir': SHOP, 'state': 'containers_removed_named_volumes_kept'}}
        project([removed, image, compose, waiting])
        preview = results(run('dryrun'))
        assert preview['removed']['change'] == 'unchanged' and all(preview[name]['status'] == 'not_checked' for name in ('image', 'compose', 'waiting')), preview
        missing = 'docker is not installed; install the package that provides it in an earlier node task'
        for task in (image, compose, waiting):
            project([removed, task])
            refused = results(run('run', 1))
            assert refused['removed']['change'] == 'unchanged', refused
            failed_once(refused[task['id']], missing)
        mark('without the docker command a task fails once and names it, a dryrun reports not checked, and a removal reports unchanged')

        # Docker: from Docker's own repository where it has one for the
        # distribution (the example docker-host on Debian 12 and Ubuntu
        # 24.04), and from the distribution's own packages elsewhere. Debian
        # 13 and Ubuntu 26.04 have Docker's repository too; their own packages
        # bring the two older versions of Docker Compose that many servers run.
        installing = time.monotonic()
        docker_host_example = (os_id, version_id) in (('ubuntu', '24.04'), ('debian', '12'))
        if docker_host_example:
            host = vm.ROOT / 'docker-host-example'
            if host.exists():
                shutil.rmtree(host)
            shutil.copytree(vm.REPO / 'examples/docker-host', host)
            vm.write_inventory(host, {'nodes': {'docker01': node}}, name='example')

            def docker_host(command, timeout=300):
                return mmx_run(command, ['docker_host'], host, None, timeout)['entries'][0]['response']
            assert docker_host('dryrun')['change'] in ('would_change', 'unknown') and not remote('command -v docker || true')
            docker_host('run', timeout=1200)
            assert docker_host('run')['change'] == 'unchanged'
            assert remote("dpkg-query -W -f '${Version}' docker-ce").startswith('5:29.')
            assert json.loads(docker("info --format '{{json .LiveRestoreEnabled}}'")) is True and docker("info --format '{{.LoggingDriver}}'") == 'json-file'
            assert remote('id -nG deploy').split().count('docker') == 1
            assert remote('systemctl is-enabled docker-image-cleanup.timer; systemctl is-active docker-image-cleanup.timer').split() == ['enabled', 'active']
        else:
            if os_id == 'debian':
                # Debian 13's own packages, with its Docker Compose 2.26.1.
                tasks = [{'apt_package': {'packages': [{'name': 'docker.io', 'version': 'newest_when_first_installed'}, {'name': 'docker-cli', 'version': 'newest_when_first_installed'}, {'name': 'docker-compose', 'version': 'newest_when_first_installed'}]}}]
            elif os_id == 'ubuntu':
                # Ubuntu 26.04's own packages, with its Docker Compose 2.40.
                tasks = [{'apt_package': {'packages': [{'name': 'docker.io', 'version': 'newest_when_first_installed'}, {'name': 'docker-compose-v2', 'version': 'newest_when_first_installed'}]}}]
            else:
                tasks = [{'dnf_repository': {'name': 'docker-ce-stable', 'description': 'Docker CE Stable',
                                             'baseurl': ['https://download.docker.com/linux/centos/$releasever/$basearch/stable'],
                                             'signing_key_url': 'https://download.docker.com/linux/centos/gpg', 'signing_key_sha256': DOCKER_RPM_KEY_SHA256}},
                         {'dnf_package': {'packages': [{'name': 'docker-ce', 'version': 'newest_when_first_installed'}, {'name': 'docker-ce-cli', 'version': 'newest_when_first_installed'}, {'name': 'containerd.io', 'version': 'newest_when_first_installed'}, {'name': 'docker-compose-plugin', 'version': 'newest_when_first_installed'}]}}]
                # The Red Hat family 10 ships the kernel modules of Docker's
                # firewall rules in kernel-modules-extra. Docker's packages
                # bring that package only for the newest kernel in the
                # repositories, and a guest can run an older one. The guest
                # gets the package for the kernel it runs, as the run-containers page
                # says.
                if remote('modprobe --dry-run xt_addrtype >/dev/null 2>&1 || echo missing') == 'missing':
                    tasks.insert(0, {'dnf_package': {'packages': [{'name': 'kernel-modules-extra-{{ .node.kernel.release }}', 'version': 'newest_when_first_installed'}]}})
            project(tasks + [START_DOCKER])
            run('run', timeout=1200)
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        installed_after = time.monotonic() - installing
        containerd_store = 'io.containerd.snapshotter' in docker("info --format '{{json .DriverStatus}}'")
        compose_version = docker('compose version --short')
        print(f'Docker {docker("version --format {{.Server.Version}}")} with the '
              f'{"containerd" if containerd_store else "classic"} image store and Docker Compose {compose_version} '
              f'on {os_id} {version_id}, installed in {installed_after:.0f} s', flush=True)
        if (os_id, version_id) == ('debian', '13'):
            assert compose_version.startswith('2.26.1') and not containerd_store, compose_version
        if (os_id, version_id) == ('ubuntu', '26.04'):
            assert compose_version.startswith('2.40.'), compose_version

        def image_id(published):
            """The image ID docker gives a published image: in the containerd
            image store the digest of its manifest, in the classic store the
            digest of its configuration."""
            return published['manifest_digest'] if containerd_store else published['image_id']

        def predicted_id(published):
            """What a dryrun can say of an image it would pull: the classic
            image store learns the ID only with the pull."""
            return published['manifest_digest'] if containerd_store else None

        # The Compose plugin and the daemon, each missing in its turn.
        plugin = docker("info --format '{{range .ClientInfo.Plugins}}{{if eq .Name \"compose\"}}{{.Path}}{{end}}{{end}}'")
        assert plugin.startswith('/'), plugin
        project([compose])
        remote(f'mv {plugin} {plugin}.set-aside')
        try:
            failed_once(results(run('run', 1))['compose'], 'docker_compose_project: the Docker Compose plugin is not installed')
            assert results(run('dryrun'))['compose']['status'] == 'not_checked'
            remote(f"cat > {plugin} <<'END'\n#!/bin/sh\n"
                   'echo \'{"SchemaVersion": "0.1.0", "Vendor": "MMX test", "Version": "v2.24.6", "ShortDescription": "an old Docker Compose"}\'\n'
                   f'END\nchmod 755 {plugin}')
            failed_once(results(run('run', 1))['compose'], 'Docker Compose 2.24.6 is installed, and MMX needs Docker Compose 2.26.1 or newer')
        finally:
            remote(f'mv -f {plugin}.set-aside {plugin}')
        remote('systemctl stop docker.socket docker.service 2>/dev/null || systemctl stop docker.service')
        try:
            project([image])
            failed_once(results(run('run', 1))['image'], 'docker_image: the Docker daemon does not answer', 'start it in an earlier node task')
            assert results(run('dryrun'))['image']['status'] == 'not_checked'
        finally:
            remote('systemctl start docker.service')
        mark('a missing Compose plugin, a Compose older than 2.26.1, and a stopped daemon each fail once with a message that names it')

        # docker_image: when_missing pulls once and never follows a tag;
        # always follows it, and a dryrun asks the registry without a pull.
        def images(pull_moving='always'):
            return [{'id': 'pinned', 'docker_image': {'image': pinned['reference'], 'pull_image': 'when_missing'}},
                    {'id': 'moving', 'docker_image': {'image': moving['reference'], 'pull_image': pull_moving}}]
        project(images())
        registry.clear_requests()
        preview = results(run('dryrun'))
        assert preview['pinned']['change'] == 'would_change' and preview['pinned']['value'] == {'image_id': None, 'previous_image_id': None}, preview
        assert preview['moving']['change'] == 'would_change' and preview['moving']['value'] == {'image_id': predicted_id(moving), 'previous_image_id': None}, preview
        assert preview['moving']['reason'] == (f'the registry now serves {moving["reference"]} under the digest {moving["manifest_digest"]}, '
                                               'and the node does not have the image'), preview
        no_image_layer_was_fetched()
        assert [r for r in registry.requests() if r['kind'] == 'manifest'], 'the dryrun did not ask the registry'
        assert image_on_node(pinned['reference']) is None and image_on_node(moving['reference']) is None
        pulled = results(run('run'))
        for name, published in (('pinned', pinned), ('moving', moving)):
            assert pulled[name]['change'] == 'changed' and pulled[name]['value'] == {'image_id': image_id(published), 'previous_image_id': None}, pulled
            assert image_on_node(published['reference']) == image_id(published)
        assert [r for r in registry.requests() if r['blob_kind'] == 'image_layer'], 'the run pulled no image layer'
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('docker_image pulls a missing image and reports the image ID that docker reports; a dryrun pulls nothing; reruns unchanged')

        first = dict(pinned=pinned, moving=moving)
        pinned = registry.publish('mmx-test/pinned', '1', image_version='2')
        moving = registry.publish('mmx-test/moving', 'stable', image_version='2')
        registry.clear_requests()
        preview = results(run('dryrun'))
        assert preview['pinned']['change'] == 'unchanged', preview
        assert preview['moving']['change'] == 'would_change' and preview['moving']['value'] == {
            'image_id': predicted_id(moving), 'previous_image_id': image_id(first['moving'])}, preview
        no_image_layer_was_fetched()
        assert image_on_node(moving['reference']) == image_id(first['moving'])
        followed = results(run('run'))
        assert followed['pinned']['change'] == 'unchanged' and image_on_node(pinned['reference']) == image_id(first['pinned']), followed
        assert followed['moving']['change'] == 'changed' and followed['moving']['value'] == {
            'image_id': image_id(moving), 'previous_image_id': image_id(first['moving'])}, followed
        assert image_on_node(moving['reference']) == image_id(moving)
        assert unchanged(run('run'))
        mark('a moved tag: when_missing keeps the image, always pulls the new one, and a dryrun with always reports both image IDs '
             'where docker can know them, and fetches no image layer')

        # A registry that does not answer, and a reference without a tag.
        project([{'id': 'unreachable', 'docker_image': {'image': '127.0.0.1:5999/mmx-test/app:1', 'pull_image': 'always'}}])
        unreachable = results(run('dryrun'))['unreachable']
        assert unreachable['status'] == 'not_checked' and '127.0.0.1:5999' in unreachable['reason'], unreachable
        project([{'docker_image': {'image': REGISTRY + '/mmx-test/app', 'pull_image': 'always'}}])
        refused = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'names no tag and no digest' in refused.stderr, refused
        mark('a dryrun with always reports not checked with the error when the registry does not answer; a reference without a tag or digest is rejected')

        # A private registry. A sampler reads every process command line
        # while the logins are in use. It looks for the password and for the
        # form in which docker stores a login: base64 of user:password.
        stored_login = base64.b64encode(f'{image_registry.USERNAME}:{image_registry.PASSWORD}'.encode()).decode()
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'mkdir -p {SAMPLES} && cat > {SAMPLES}/patterns')], input=f'{image_registry.PASSWORD}\n{stored_login}\ndocker pull\n')
        remote(f"""cat > {SAMPLES}/sampler <<'SAMPLER'
while test -e {SAMPLES}/patterns && test ! -e {SAMPLES}/stop-sampler; do
  cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\000' ' ' | grep -a -o -F -f {SAMPLES}/patterns >> {SAMPLES}/command-lines
done
SAMPLER
touch {SAMPLES}/command-lines
nohup sh {SAMPLES}/sampler >/dev/null 2>&1 &""")
        private = {'id': 'private', 'docker_image': {'image': secret['reference'], 'pull_image': 'always'}}
        project([private])
        without_login = results(run('dryrun'))['private']
        assert without_login['status'] == 'not_checked' and without_login['reason'].startswith(
            f'docker_image: the registry does not say which image {secret["reference"]} names: '), without_login
        assert ("If the image is private, a dryrun needs the registry login from registry_username and registry_password "
                'on the node task') in without_login['reason'], without_login
        refused = results(run('run', 1))['private']
        assert refused['status'] == 'failed' and image_on_node(secret['reference']) is None, refused
        # With the login, whose password is a secret. MMX hands the login
        # only to docker, so it does not hide the task's result. With a wrong
        # password the registry refuses, and the result gives its reason.
        private['docker_image'].update(registry_username=image_registry.USERNAME, registry_password='{{ .secrets.registry_password }}')
        project([private], **login)
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = 'not-the-password'
        wrong_password = 'incorrect username or password'  # the registry's own words
        wrong = results(run('dryrun'))['private']
        assert wrong['status'] == 'not_checked' and wrong.get('secret') is not True and wrong['reason'].startswith(
            f'docker_image: the registry does not say which image {secret["reference"]} names: ') and wrong_password in wrong['reason'], wrong
        wrong = results(run('run', 1))['private']
        assert wrong['status'] == 'failed' and wrong.get('secret') is not True and (
            f'docker_image: docker pull --quiet {secret["reference"]}: ' in wrong['reason']) and wrong_password in wrong['reason'], wrong
        assert image_on_node(secret['reference']) is None
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = image_registry.PASSWORD
        registry.clear_requests()
        preview = results(run('dryrun'))['private']
        assert preview['status'] == 'succeeded' and preview['change'] == 'would_change' and preview.get('secret') is not True, preview
        assert preview['value'] == {'image_id': predicted_id(secret), 'previous_image_id': None}, preview
        assert [r for r in registry.requests() if r['authorized'] and r['kind'] == 'manifest'], registry.requests()
        no_image_layer_was_fetched()
        assert image_on_node(secret['reference']) is None
        # Every test image has the same layer, which the node has by now, so
        # the pull asks only for the manifest, with the login.
        registry.clear_requests()
        pulled = results(run('run'))['private']
        assert pulled['change'] == 'changed' and pulled.get('secret') is not True, pulled
        assert pulled['value'] == {'image_id': image_id(secret), 'previous_image_id': None} and image_on_node(secret['reference']) == image_id(secret), pulled
        assert [r for r in registry.requests() if r['authorized'] and r['kind'] == 'manifest' and r['status'] == 200], registry.requests()
        for command in ('run', 'dryrun'):
            assert results(run(command))['private']['change'] == 'unchanged'
        mark('a private registry refuses a task without its login and serves one with registry_username and registry_password, in a dryrun too; '
             'the result of the task with the login names the image, and with a wrong password it gives the registry\'s reason')

        # One image for a container that systemd runs: the image ID is a
        # tracked input, so the service restarts once for a new image, also
        # after a run that pulled it and ended before the restart.
        converter = registry.publish('mmx-test/converter', 'stable', image_version='1')

        def converter_block(validate='true', verify=True):
            return [{'task_block': {
                'id': 'converter',
                'changes': [
                    {'id': 'converter_image', 'docker_image': {'image': converter['reference'], 'pull_image': 'always'}},
                    {'id': 'converter_unit', 'systemd_unit': {'name': CONVERTER + '.service', 'content':
                        '[Unit]\nRequires=docker.service\nAfter=docker.service\n\n[Service]\n'
                        f'ExecStartPre=-/usr/bin/docker rm --force {CONVERTER}\n'
                        f'ExecStart=/usr/bin/docker run --rm --name {CONVERTER} --publish 127.0.0.1:{CONVERTER_PORT}:8080 {converter["reference"]}\n'
                        f'ExecStop=/usr/bin/docker stop {CONVERTER}\nRestart=always\n\n[Install]\nWantedBy=multi-user.target\n'}}],
                'validate': [{'shell': {'script': validate}}],
                'activate': [{'id': 'converter_service', 'systemd_service': {'name': CONVERTER, 'state': 'running', 'start_at_boot': True}}],
                'verify': [{'wait_for': {'url_responds': f'http://127.0.0.1:{CONVERTER_PORT}/health', 'wait_at_most': '30s'}}] if verify else [],
            }}]
        project(converter_block())
        started_service = changes(run('run'))
        assert started_service['converter_image'] == 'changed' and started_service['converter_service'] == 'changed', started_service
        running = answer(CONVERTER_PORT)
        assert running['image_version'] == '1', running
        assert unchanged(run('run')) and answer(CONVERTER_PORT) == running
        converter = registry.publish('mmx-test/converter', 'stable', image_version='2')
        preview = results(run('dryrun'))
        assert preview['converter_image']['change'] == 'would_change' and preview['converter_service']['change'] == 'would_change', preview
        assert answer(CONVERTER_PORT) == running
        restarted = changes(run('run'))
        assert restarted['converter_image'] == 'changed' and restarted['converter_service'] == 'changed' and restarted['converter_unit'] == 'unchanged', restarted
        running = answer(CONVERTER_PORT, was=running)
        assert running['image_version'] == '2', running
        converter = registry.publish('mmx-test/converter', 'stable', image_version='3')
        project(converter_block(validate='false'))
        run('run', 1)
        assert image_on_node(converter['reference']) == image_id(converter) and answer(CONVERTER_PORT) == running
        project(converter_block())
        recovered = changes(run('run'))
        assert recovered['converter_image'] == 'unchanged' and recovered['converter_service'] == 'changed', recovered
        running = answer(CONVERTER_PORT, was=running)
        assert running['image_version'] == '3', running
        assert unchanged(run('run')) and answer(CONVERTER_PORT) == running
        # The container declares no health check, so the wait for its health
        # ends at once instead of after wait_at_most.
        project([{'id': 'converter_health', 'wait_for': {'docker_container_is_healthy': CONVERTER, 'wait_at_most': '10m'}}])
        asked = time.monotonic()
        no_check = results(run('run', 1))['converter_health']
        assert no_check['status'] == 'failed' and f'the container {CONVERTER} declares no health check' in no_check['reason'], no_check
        assert time.monotonic() - asked < 60 and results(run('dryrun'))['converter_health']['status'] == 'not_checked'
        remote(f'systemctl disable --now {CONVERTER}.service\nrm /etc/systemd/system/{CONVERTER}.service\nsystemctl daemon-reload')
        mark('a container that systemd runs restarts once when its image ID changes, also after a run that pulled the image and ended '
             'before the restart; a dryrun predicts the restart; the wait for a container without a health check fails at once')

        # The example deploy-with-docker-compose, with the test image in
        # place of nginx: its inventory holds what is specific to the
        # application.
        registry.publish('mmx-test/shop', '1', image_version='1')
        registry.publish('mmx-test/shop', '2', image_version='2')
        example = vm.ROOT / 'deploy-with-docker-compose-example'
        if example.exists():
            shutil.rmtree(example)
        shutil.copytree(vm.REPO / 'examples/deploy-with-docker-compose', example)

        def shop(command, playbook='deploy', expected=None, **values):
            variables = {'shop_image': f'{REGISTRY}/mmx-test/shop:1', 'shop_health_check': '{test: ' + HEALTH_CHECK + ', interval: 1s}',
                         'shop_config_dir_in_container': '/etc/container-test-program', 'shop_config_file_name': 'configuration',
                         'shop_config': 'first configuration\n', 'shop_reload_command': '/container-test-program reload-configuration',
                         'shop_time_zone': 'Europe/Berlin', **values}
            vm.write_inventory(example, {'nodes': {'shop01': node}, 'vars': variables}, name='example')
            return mmx_run(command, [playbook], example, expected, 300)

        def data_file(name):
            return f'/var/lib/docker/volumes/shop_web_data/_data/{name}'
        preview = shop('dryrun')
        assert preview['entries'][0]['response']['change'] in ('would_change', 'unknown') and not remote(f'ls -d {EXAMPLE_SHOP} 2>/dev/null || true')
        deployed = shop('run')
        assert [services(r) for r in project_result(deployed)] == [{'web': 'created'}]
        assert changes(deployed)['reload_shop_config'] == 'changed'
        running = answer(EXAMPLE_PORT)
        assert running['image_version'] == '1' and running['configuration'] == 'first configuration\n' and running['configuration_reloads'] == 1, running
        assert unchanged(shop('run')) and answer(EXAMPLE_PORT) == running
        assert unchanged(shop('dryrun'))
        # The mounted file: the application reads it again, in the same
        # container.
        reloaded = changed(shop('run', shop_config='second configuration\n'))
        assert reloaded == {'shop_config', 'shop_config/changes/task_1', 'shop_config/activate/reload_shop_config'}, reloaded
        assert answer(EXAMPLE_PORT) == {**running, 'configuration': 'second configuration\n', 'configuration_reloads': 2}
        # The environment file, then the image: each recreates the container,
        # and a dryrun cannot compare before the file is installed.
        second = dict(shop_config='second configuration\n')
        preview = shop('dryrun', shop_time_zone='Europe/Vienna', **second)
        compose_preview = project_result(preview)[0]
        assert compose_preview['change'] == 'would_change' and compose_preview['value']['services'] == [
            {'name': 'web', 'action': 'would_change', 'reason': FILES_WOULD_CHANGE}], compose_preview
        assert answer(EXAMPLE_PORT)['process_started_at'] == running['process_started_at']
        recreated = shop('run', shop_time_zone='Europe/Vienna', **second)
        assert [services(r) for r in project_result(recreated)] == [{'web': 'recreated'}]
        running = answer(EXAMPLE_PORT, was=running)
        assert docker('compose --project-directory /opt/shop exec --no-TTY web /container-test-program check-health; echo exit $?').endswith('exit 0')
        assert remote("docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' shop-web-1 | grep ^TZ=") == 'TZ=Europe/Vienna'
        newer = dict(shop_image=f'{REGISTRY}/mmx-test/shop:2', shop_time_zone='Europe/Vienna', **second)
        upgraded = shop('run', **newer)
        assert [services(r) for r in project_result(upgraded)] == [{'web': 'recreated'}]
        running = answer(EXAMPLE_PORT, was=running)
        assert running['image_version'] == '2' and running['configuration'] == 'second configuration\n', running
        assert unchanged(shop('run', **newer))
        mark('the example deploy-with-docker-compose deploys, reruns unchanged, reloads a changed mounted file in the same container, '
             'and recreates the container for a changed environment file and for another image')

        # Stop for a backup and start again; remove and keep the data;
        # deploy again; remove and delete the data.
        starts = int(remote(f'wc -l < {data_file("starts.log")}'))
        backed_up = shop('run', 'back_up_data', **newer)
        assert [services(r) for r in project_result(backed_up)] == [{'web': 'stopped'}, {'web': 'started'}]
        assert './starts.log' in remote('tar --list --file /srv/shop-backups/web_data.tar').split()
        assert int(remote(f'wc -l < {data_file("starts.log")}')) == starts + 1 and answer(EXAMPLE_PORT, was=running)['image_version'] == '2'
        kept = shop('run', 'remove_and_keep_data', **newer)
        assert [services(r) for r in project_result(kept)] == [{'web': 'removed'}]
        assert container_ids(EXAMPLE_SHOP) == [] and remote('docker volume ls --quiet') == 'shop_web_data' and docker('network ls --quiet --filter label=com.docker.compose.project=shop') == ''
        assert shop('run', 'remove_and_keep_data', **newer)['entries'][0]['response']['change'] == 'unchanged'
        shop('run', **newer)
        assert int(remote(f'wc -l < {data_file("starts.log")}')) == starts + 2, 'the data of the named volume did not stay'
        deleted = shop('run', 'remove_and_delete_data', **newer)
        assert [services(r) for r in project_result(deleted)] == [{'web': 'removed'}]
        assert container_ids(EXAMPLE_SHOP) == [] and remote('docker volume ls --quiet') == '' and not remote(f'ls -d {EXAMPLE_SHOP} 2>/dev/null || true')
        assert image_on_node(f'{REGISTRY}/mmx-test/shop:2'), 'a removal deleted an image'
        assert shop('run', 'remove_and_delete_data', **newer)['entries'][0]['response']['change'] == 'unchanged'
        assert shop('dryrun', 'remove_and_delete_data', **newer)['entries'][0]['response']['change'] == 'unchanged'
        mark('the example stops the project for a backup and starts it again, removes it and keeps the named volume, and removes it '
             'with the volume; each removal reruns unchanged, also when the directory is gone')

        # A project that someone brought up by hand, as on a server that MMX
        # takes over, with an environment file. MMX keeps no record of its
        # own: a dryrun asks Docker Compose's own dry run. The first dryrun
        # predicts what the first run then does, which is nothing.
        adopted = '/opt/mmx-test-adopted'

        def write_adopted(greeting, store_mark):
            compose_file = ('services:\n'
                            '  store:\n'
                            f'    image: {pinned["reference"]}\n'
                            f'    environment: {{MARK: "{store_mark}"}}\n'
                            '  web:\n'
                            f'    image: {pinned["reference"]}\n'
                            '    env_file: [web.env]\n'
                            '    depends_on: {store: {condition: service_started, restart: true}}\n')
            remote(f"mkdir -p {adopted}\nprintf 'GREETING={greeting}\\n' > {adopted}/web.env\ncat > {adopted}/compose.yaml <<'END'\n{compose_file}END\n")

        write_adopted('hello', 1)
        remote(f'cd {adopted}\ndocker compose up --detach --quiet-pull 2>/dev/null')
        by_hand = container_ids(adopted)
        assert [c.split()[1] for c in by_hand] == ['running', 'running'], by_hand
        project([{'id': 'project', 'docker_compose_project': {'compose_project_name': 'mmx-test-adopted', 'compose_project_dir': adopted, 'state': 'running', 'pull_images': 'when_missing',
                                                              'containers_of_removed_services': 'remove'}}])
        first = results(run('dryrun'))['project']
        assert first['status'] == 'succeeded' and first['change'] == 'unchanged' and services(first) == {'store': 'unchanged', 'web': 'unchanged'}, first
        assert unchanged(run('run')) and container_ids(adopted) == by_hand
        assert unchanged(run('dryrun')) and container_ids(adopted) == by_hand
        # A changed environment file recreates its service, and the dryrun
        # says so before.
        write_adopted('good evening', 1)
        preview = results(run('dryrun'))['project']
        assert preview['value']['services'] == [{'name': 'store', 'action': 'unchanged'},
                                                {'name': 'web', 'action': 'would_recreate', 'reason': 'the definition differs'}], preview
        assert container_ids(adopted) == by_hand
        assert services(results(run('run'))['project']) == {'store': 'unchanged', 'web': 'recreated'} and container_ids(adopted)[0] == by_hand[0]
        assert unchanged(run('dryrun'))
        # A changed service that another one depends on with restart: true.
        # Compose 2.26 recreates both; later versions recreate the one and
        # stop and start the other. The dryrun predicts what the run of the
        # node's Compose then does.
        write_adopted('good evening', 2)
        before_store = container_ids(adopted)
        preview = results(run('dryrun'))['project']
        predicted = {s['name']: s for s in preview['value']['services']}
        assert predicted['store'] == {'name': 'store', 'action': 'would_recreate', 'reason': 'the definition differs'}, preview
        assert predicted['web']['reason'] == {
            'would_recreate': 'the definition differs, or Docker Compose recreates the service together with store, which it depends on',
            'would_start': 'Docker Compose stops and restarts the container because Compose recreates a service or network used by the container',
        }.get(predicted['web']['action']), preview
        assert container_ids(adopted) == before_store
        assert services(results(run('run'))['project']) == {name: DONE[action] for name, action in services(preview).items()}, preview
        assert unchanged(run('dryrun'))
        # A service that must complete before another one starts. The start
        # step of Compose's dry run waits for such a service, on Compose 2
        # without an end when its container is new; MMX asks for the creation
        # step alone.
        ordered = '/opt/mmx-test-ordered'

        def write_ordered(migrate_exit_code, web_waits_for_migrate):
            compose_file = (f"services:\n  migrate:\n    image: {pinned['reference']}\n    environment: {{EXIT_AT_START_WITH_CODE: \"{migrate_exit_code}\"}}\n"
                            f"  web:\n    image: {pinned['reference']}\n")
            if web_waits_for_migrate:
                compose_file += '    depends_on: {migrate: {condition: service_completed_successfully}}\n'
            remote(f"mkdir -p {ordered}\ncat > {ordered}/compose.yaml <<'END'\n{compose_file}END\n")

        write_ordered(0, True)
        ordered_project = {'id': 'project', 'docker_compose_project': {'compose_project_name': 'mmx-test-ordered', 'compose_project_dir': ordered, 'state': 'running', 'pull_images': 'when_missing',
                                                                       'containers_of_removed_services': 'remove'}}
        # With a slash at its end the directory is the same project.
        ordered_health = {'id': 'healthy', 'wait_for': {'docker_compose_project_is_ready': ordered + '/', 'wait_at_most': '10s'}}
        project([ordered_project])
        preview = results(run('dryrun', timeout=60))['project']
        assert preview['value']['services'] == [{'name': 'migrate', 'action': 'would_create', 'reason': 'the container is missing'},
                                                {'name': 'web', 'action': 'would_create', 'reason': 'the container is missing'}], preview
        assert container_ids(ordered) == []
        # The service that ran once and ended with exit code 0 has completed:
        # the project is healthy. One that ended with another code is not.
        project([ordered_project, ordered_health])
        completed = results(run('run'))
        assert services(completed['project']) == {'migrate': 'created', 'web': 'created'}, completed
        assert completed['healthy']['status'] == 'succeeded' and 'runs or has completed' in completed['healthy']['reason'], completed
        assert [c.split()[1] for c in container_ids(ordered)] == ['exited', 'running'], container_ids(ordered)
        write_ordered(3, False)
        ended_badly = results(run('run', 1))
        assert 'mmx-test-ordered-migrate-1 declares no health check and is exited with code 3, not running' in ended_badly['healthy']['reason'], ended_badly
        remote(f'cd {adopted}\ndocker compose down 2>/dev/null\ncd {ordered}\ndocker compose down 2>/dev/null\nrm -r {adopted} {ordered}')
        mark('a project that was brought up by hand with an environment file: the first dryrun predicts no recreation and the run keeps the containers; '
             'a changed environment file and a changed service that another one depends on are predicted as the run does them; '
             'a service that must complete first does not hold up the dryrun, and when it ended with exit code 0 the project is healthy')

        # The suite's own project: a web service with a health check and a
        # worker without one.
        web_image = registry.publish('mmx-test/web', 'stable', image_version='1')
        worker_image = registry.publish('mmx-test/worker', '1', image_version='1')

        def shop_block(pull_images='always', worker=True, greeting='hello', validate='true', removed_services='remove', wait='30s'):
            compose_file = (
                'services:\n'
                '  web:\n'
                f'    image: {web_image["reference"]}\n'
                '    restart: unless-stopped\n'
                f'    ports: ["127.0.0.1:{SHOP_PORT}:8080"]\n'
                '    env_file: [web.env]\n'
                '    volumes: ["web_data:/data"]\n'
                '    healthcheck:\n'
                f'      test: {HEALTH_CHECK}\n'
                '      interval: 1s\n'
                '      retries: 1\n')
            if worker:
                compose_file += f'  worker:\n    image: {worker_image["reference"]}\n    restart: unless-stopped\n'
            compose_file += 'volumes:\n  web_data:\n    name: mmx-test-shop_web_data\n'
            return [{'task_block': {
                'id': 'shop',
                'changes': [
                    {'directory': {'path': SHOP, 'permissions_mode': '0755'}},
                    {'id': 'environment_file', 'file': {'path': SHOP + '/web.env', 'permissions_mode': '0600', 'content': f'GREETING={greeting}\n'}},
                    {'id': 'compose_file', 'file': {'path': SHOP + '/compose.yaml', 'permissions_mode': '0644', 'content': compose_file,
                                                    'check_new_file': {'shell': {'script': f'docker compose --project-directory {SHOP} --file "$NEW_FILE" config --quiet',
                                                                                 'env': {'NEW_FILE': '{{ .new_file.path }}'}}}}}],
                'validate': [{'shell': {'script': validate}}],
                'activate': [{'id': 'project', 'docker_compose_project': {
                    'compose_project_name': 'mmx-test-shop', 'compose_project_dir': SHOP, 'state': 'running', 'pull_images': pull_images, 'containers_of_removed_services': removed_services}}],
                'verify': [{'id': 'healthy', 'wait_for': {'docker_compose_project_is_ready': SHOP, 'wait_at_most': wait}}],
            }}]

        def shop_state(state):
            task = {'compose_project_name': 'mmx-test-shop', 'compose_project_dir': SHOP, 'state': state}
            if state == 'containers_and_named_volumes_removed':
                # This state requires the project's name: the one Compose
                # gives SHOP by itself.
                task['compose_project_name'] = 'mmx-test-shop'
            return [{'id': 'project', 'docker_compose_project': task}]

        project(shop_block())
        preview = results(run('dryrun'))
        assert preview['project']['change'] == 'would_change' and preview['project']['value'] == {'services': [], 'images': []}, preview
        assert preview['project']['reason'].startswith(FILES_WOULD_CHANGE) and preview['healthy']['status'] == 'not_checked', preview
        registry.clear_requests()
        created = results(run('run'))
        assert services(created['project']) == {'web': 'created', 'worker': 'created'} and created['healthy']['status'] == 'succeeded', created
        assert created['project']['value']['images'] == [
            {'image': web_image['reference'], 'previous_image_id': None, 'image_id': image_id(web_image)},
            {'image': worker_image['reference'], 'previous_image_id': None, 'image_id': image_id(worker_image)}], created
        running = answer(SHOP_PORT)
        containers = container_ids(SHOP)
        assert running['image_version'] == '1' and [c.split()[1] for c in containers] == ['running', 'running'], (running, containers)
        registry.clear_requests()
        for command in ('run', 'dryrun'):
            rerun = results(run(command))
            assert rerun['project']['change'] == 'unchanged' and services(rerun['project']) == {'web': 'unchanged', 'worker': 'unchanged'}, rerun
            assert rerun['healthy']['status'] == 'succeeded' and rerun['compose_file']['change'] == 'unchanged', rerun
        assert container_ids(SHOP) == containers and answer(SHOP_PORT) == running
        assert [r for r in registry.requests() if r['kind'] == 'manifest'], 'always did not ask the registry'
        no_image_layer_was_fetched()
        mark('docker_compose_project creates the containers of a project, lists the pulled images, and reruns unchanged with '
             'pull_images: always; the dryrun before the Compose file exists says that an earlier node task would change the project\'s files')

        # A second project directory with the same last name. Docker Compose
        # names a project after the last part of its directory's path, so it
        # would take both directories for one project: up would remove the
        # first project's containers, and a removal would delete its volume,
        # which has the name that the second project's files declare. The task
        # fails instead, is not tried again, and changes nothing. The removal
        # must name its project; it gives the first project's name.
        twin = '/opt/mmx-test-twin/mmx-test-shop'
        remote(f"mkdir -p {twin}/parts\ncat > {twin}/parts/base.yaml <<'END'\nservices:\n  twin:\n    image: {worker_image['reference']}\n"
               f"    volumes: [\"web_data:/data\"]\nvolumes:\n  web_data:\nEND\n"
               f"cat > {twin}/production.yaml <<'END'\nservices:\n  twin:\n    environment: {{GREETING: from the second file}}\nEND\n")

        def twin_project(state='running', **more):
            task = {'compose_project_name': 'mmx-test-shop', 'compose_project_dir': twin, 'compose_files': ['parts/base.yaml', 'production.yaml'], 'state': state, **more}
            if state == 'running':
                task.update(pull_images='when_missing', containers_of_removed_services='remove')
            return [{'id': 'twin', 'docker_compose_project': task}]

        def first_project_is_untouched():
            assert container_ids(SHOP) == containers and answer(SHOP_PORT) == running and remote('docker volume ls --quiet') == 'mmx-test-shop_web_data'

        for tasks in (twin_project(), twin_project('containers_and_named_volumes_removed', compose_project_name='mmx-test-shop')):
            project(tasks)
            for command in ('dryrun', 'run'):
                refused = results(run(command, 1))['twin']
                assert refused['status'] == 'failed' and refused.get('failure_kind') == 'config' and len(refused.get('tries') or [refused]) == 1, refused
                assert f'the project of {twin} and the project of {SHOP} have the same project name, mmx-test-shop' in refused['reason'], refused
                assert 'Give each project its own compose_project_name' in refused['reason'], refused
                first_project_is_untouched()
        # With its own compose_project_name, and with two Compose files, the
        # second project runs beside the first and is removed alone.
        project(twin_project(compose_project_name='mmx-test-twin'))
        assert services(results(run('run'))['twin']) == {'twin': 'created'}
        labels = json.loads(docker("inspect --format '{{json .Config.Labels}}' mmx-test-twin-twin-1"))
        assert labels['com.docker.compose.project'] == 'mmx-test-twin' and labels['com.docker.compose.project.working_dir'] == twin, labels
        assert 'GREETING=from the second file' in docker("inspect --format '{{range .Config.Env}}{{println .}}{{end}}' mmx-test-twin-twin-1").splitlines()
        assert sorted(remote('docker volume ls --quiet').split()) == ['mmx-test-shop_web_data', 'mmx-test-twin_web_data']
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        project(twin_project('containers_and_named_volumes_removed', compose_project_name='mmx-test-twin'))
        preview = results(run('dryrun'))['twin']
        assert preview['value']['services'] == [{'name': 'twin', 'action': 'would_remove', 'reason': 'the service has containers'}], preview
        assert preview['reason'] == 'would delete the named volumes mmx-test-twin_web_data with the data in them', preview
        gone = results(run('run'))['twin']
        assert services(gone) == {'twin': 'removed'} and gone['reason'] == 'deleted the named volumes mmx-test-twin_web_data with the data in them', gone
        assert container_ids(twin) == [] and unchanged(run('run'))
        first_project_is_untouched()
        remote('rm -r /opt/mmx-test-twin')
        mark('a second project directory with the same last name fails the task without a change to the first project; with compose_project_name '
             'and two compose_files it runs beside the first one and is removed alone, with the volume that the result names')

        # The registry moves the web image's tag.
        first_web, web_image = web_image, registry.publish('mmx-test/web', 'stable', image_version='2')
        project(shop_block(pull_images='when_missing'))
        assert unchanged(run('run')) and answer(SHOP_PORT) == running
        project(shop_block())
        registry.clear_requests()
        preview = results(run('dryrun'))['project']
        assert preview['change'] == 'would_change' and preview['value']['services'] == [
            {'name': 'web', 'action': 'would_recreate', 'reason': f'the image differs: the registry names another image for {web_image["reference"]} now'},
            {'name': 'worker', 'action': 'unchanged'}], preview
        assert preview['value']['images'] == [{'image': web_image['reference'], 'previous_image_id': image_id(first_web), 'image_id': predicted_id(web_image)}], preview
        no_image_layer_was_fetched()
        assert container_ids(SHOP) == containers and image_on_node(web_image['reference']) == image_id(first_web)
        followed = results(run('run'))['project']
        assert services(followed) == {'web': 'recreated', 'worker': 'unchanged'}, followed
        assert followed['value']['images'] == [{'image': web_image['reference'], 'previous_image_id': image_id(first_web), 'image_id': image_id(web_image)}], followed
        running = answer(SHOP_PORT, was=running)
        assert running['image_version'] == '2' and container_ids(SHOP)[1] == containers[1], (running, container_ids(SHOP))
        assert unchanged(run('run'))
        mark('a moved tag in a Compose project: when_missing keeps the container, a dryrun with always names the service it would recreate '
             'and fetches no image layer, and a run pulls the image and recreates only that service')

        # A reference that names a list of images, one for each platform, as
        # Docker's official images do. The registry serves another list in
        # which the image for the node's platform is the same: a dryrun
        # cannot see that, so it claims no recreation, and no changed image
        # where the node's image store names an image by its own platform's
        # image. The run then shows what Docker and Compose do.
        platforms = '/opt/mmx-test-platforms'

        def publish_list(version_for_the_node, version_for_another_platform):
            another = 'arm64' if registry.architecture == 'amd64' else 'amd64'
            images = [(registry.architecture, image_registry.image(registry.layer, registry.architecture, version_for_the_node)),
                      (another, image_registry.image(registry.layer, another, version_for_another_platform))]
            listed = json.dumps({'schemaVersion': 2, 'mediaType': 'application/vnd.docker.distribution.manifest.list.v2+json', 'manifests': [
                {'mediaType': image_registry.MANIFEST_MEDIA_TYPE, 'size': len(image.manifest), 'digest': image.manifest_digest,
                 'platform': {'architecture': architecture, 'os': 'linux'}} for architecture, image in images]}).encode()
            files = {}
            for _, image in images:
                files['image_configurations/' + image.image_id[7:]] = image.configuration
                files['manifests/' + image.manifest_digest[7:]] = image.manifest
            files['manifests/' + image_registry.digest(listed)[7:]] = listed
            files['repositories/mmx-test/platforms/_tags/stable'] = image_registry.digest(listed).encode()
            registry.remote(f'python3 {image_registry.DIRECTORY}/registry_server.py store {image_registry.DIRECTORY}\n',
                            input=image_registry.store_input(files))
            # The ID that docker gives the reference's image: in the containerd
            # image store the list's digest, in the classic store the ID of
            # the image for the node's platform.
            return image_registry.digest(listed) if containerd_store else images[0][1].image_id

        list_reference = f'{REGISTRY}/mmx-test/platforms:stable'
        another_list = f'the registry now serves another list of images for {list_reference}'
        first_id = publish_list('1', '1')
        project([{'id': 'image', 'docker_image': {'image': list_reference, 'pull_image': 'always'}},
                 {'directory': {'path': platforms}},
                 {'file': {"permissions_mode": "0644", 'path': platforms + '/compose.yaml', 'content': f'services:\n  web:\n    image: {list_reference}\n'}},
                 {'id': 'project', 'docker_compose_project': {'compose_project_name': 'mmx-test-platforms', 'compose_project_dir': platforms, 'state': 'running', 'pull_images': 'always',
                                                              'containers_of_removed_services': 'remove'}}])
        pulled = results(run('run'))
        assert pulled['image']['value'] == {'image_id': first_id, 'previous_image_id': None} and services(pulled['project']) == {'web': 'created'}, pulled
        assert unchanged(run('dryrun'))
        kept = container_ids(platforms)
        second_id = publish_list('1', '2')
        registry.clear_requests()
        preview = results(run('dryrun'))
        web = preview['project']['value']['services'][0]
        assert web['action'] == 'not_checked' and web['reason'].startswith(another_list), preview
        if containerd_store:
            assert second_id != first_id and preview['image']['change'] == 'would_change', preview
            assert preview['image']['value'] == {'image_id': second_id, 'previous_image_id': first_id}, preview
            assert preview['project']['change'] == 'would_change', preview
            assert preview['project']['value']['images'] == [{'image': list_reference, 'previous_image_id': first_id, 'image_id': second_id}], preview
        else:
            assert second_id == first_id and preview['image']['status'] == 'not_checked', preview
            assert preview['image']['reason'].startswith('docker_image: ' + another_list), preview
            assert preview['project']['status'] == 'not_checked' and preview['project']['reason'].startswith('docker_compose_project: ' + another_list), preview
            unseen = preview['project']['value']['images']
            assert len(unseen) == 1 and unseen[0]['image_id'] is None and unseen[0]['reason'].startswith(another_list), preview
        no_image_layer_was_fetched()
        assert container_ids(platforms) == kept and image_on_node(list_reference) == first_id
        applied = results(run('run'))
        if containerd_store:
            # The image has another ID. Compose 5 keeps the container, whose
            # image for the node's platform stayed; Compose 2.40 recreates it.
            assert applied['image']['value'] == {'image_id': second_id, 'previous_image_id': first_id}, applied
            assert services(applied['project'])['web'] in ('unchanged', 'recreated'), applied
        else:
            assert applied['image']['change'] == 'unchanged' and applied['project']['change'] == 'unchanged' and container_ids(platforms) == kept, applied
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        remote(f'cd {platforms}\ndocker compose down 2>/dev/null\nrm -r {platforms}')
        mark('a reference that names a list of images for several platforms: when the registry serves another list with the same image for '
             'the node\'s platform, a dryrun reports not checked what it cannot see, and the run changes only what Docker and Compose change')

        # A run writes the environment file and ends before the containers
        # are updated; the next run finishes it.
        project(shop_block(greeting='good morning', validate='false'))
        run('run', 1)
        assert remote(f'cat {SHOP}/web.env') == 'GREETING=good morning' and answer(SHOP_PORT) == running
        project(shop_block(greeting='good morning'))
        finished = results(run('run'))
        assert finished['environment_file']['change'] == 'unchanged' and services(finished['project']) == {'web': 'recreated', 'worker': 'unchanged'}, finished
        running = answer(SHOP_PORT, was=running)
        mark('a run that wrote the environment file and ended before the containers were updated is finished by the next run')

        # A service that left the Compose file, and a stopped container.
        project(shop_block(worker=False, greeting='good morning', removed_services='keep'))
        assert services(results(run('run'))['project']) == {'web': 'unchanged', 'worker': 'unchanged'} and len(container_ids(SHOP)) == 2
        project(shop_block(worker=False, greeting='good morning'))
        docker('stop mmx-test-shop-web-1')
        preview = results(run('dryrun'))['project']
        assert preview['value']['services'] == [{'name': 'web', 'action': 'would_start', 'reason': 'the container is stopped'},
                                                {'name': 'worker', 'action': 'would_remove', 'reason': 'the service is no longer in the Compose file'}], preview
        assert len(container_ids(SHOP)) == 2
        applied = results(run('run'))
        assert services(applied['project']) == {'web': 'started', 'worker': 'removed'} and applied['healthy']['status'] == 'succeeded', applied
        assert [c.split()[1] for c in container_ids(SHOP)] == ['running']
        running = answer(SHOP_PORT, was=running)
        mark('containers_of_removed_services: keep leaves the container of a removed service and remove removes it; a stopped container is started')

        # Health: an unhealthy container fails the wait with its status and
        # the output of its last health check; both forms pass when it is
        # healthy.
        waits = [{'id': 'project_health', 'wait_for': {'docker_compose_project_is_ready': SHOP, 'wait_at_most': '20s'}},
                 {'id': 'container_health', 'wait_for': {'docker_container_is_healthy': 'mmx-test-shop-web-1', 'wait_at_most': '20s'}}]
        project(waits)
        for command in ('run', 'dryrun'):
            assert set(r['status'] for r in results(run(command)).values()) == {'succeeded'}
        unhealthy = '/var/lib/docker/volumes/mmx-test-shop_web_data/_data/unhealthy'
        remote(f'touch {unhealthy}\nuntil [ "$(docker inspect --format {{{{.State.Health.Status}}}} mmx-test-shop-web-1)" = unhealthy ]; do sleep 0.2; done')
        for wait in waits:
            project([{**wait, 'wait_for': {**wait['wait_for'], 'wait_at_most': '2s'}}])
            failed = results(run('run', 1))[wait['id']]
            assert failed['status'] == 'failed' and 'mmx-test-shop-web-1 is unhealthy' in failed['reason'] and 'its last health check exited with code 1' in failed['reason'], failed
        assert results(run('dryrun'))['container_health']['status'] == 'not_checked'
        remote(f'rm {unhealthy}')
        project(waits)
        assert set(r['status'] for r in results(run('run')).values()) == {'succeeded'}
        assert answer(SHOP_PORT) == running, 'a failed health check restarted the container'
        mark('wait_for docker_compose_project_is_ready and docker_container_is_healthy pass for a healthy project and fail for an unhealthy '
             'container with its status and its last health check; a failed health check starts nothing')

        # Stopped, running again, and the two removals.
        containers = container_ids(SHOP)
        project(shop_state('stopped'))
        assert results(run('dryrun'))['project']['value']['services'] == [{'name': 'web', 'action': 'would_stop', 'reason': 'the container runs'}]
        assert services(results(run('run'))['project']) == {'web': 'stopped'}
        assert [c.split()[1] for c in container_ids(SHOP)] == ['exited'] and [c.split()[0] for c in container_ids(SHOP)] == [c.split()[0] for c in containers]
        for command in ('run', 'dryrun'):
            assert results(run(command))['project']['change'] == 'unchanged'
        project(shop_block(worker=False, greeting='good morning'))
        assert services(results(run('run'))['project']) == {'web': 'started'} and container_ids(SHOP) == containers
        project(shop_state('containers_removed_named_volumes_kept'))
        assert results(run('dryrun'))['project']['value']['services'] == [{'name': 'web', 'action': 'would_remove', 'reason': 'the service has containers'}]
        assert container_ids(SHOP) == containers
        assert services(results(run('run'))['project']) == {'web': 'removed'}
        assert container_ids(SHOP) == [] and remote('docker volume ls --quiet') == 'mmx-test-shop_web_data'
        assert results(run('run'))['project']['change'] == 'unchanged'
        # The named volume goes with the other removal state, also when the
        # containers are gone already.
        project(shop_state('containers_and_named_volumes_removed'))
        preview = results(run('dryrun'))['project']
        assert preview['change'] == 'would_change' and 'mmx-test-shop_web_data' in preview['reason'] and remote('docker volume ls --quiet') == 'mmx-test-shop_web_data', preview
        emptied = results(run('run'))['project']
        assert emptied['change'] == 'changed' and 'deleted the named volumes mmx-test-shop_web_data' in emptied['reason'] and remote('docker volume ls --quiet') == '', emptied
        assert results(run('run'))['project']['change'] == 'unchanged'
        # A Compose file that Compose cannot load must not block a removal:
        # here the environment file that it names is gone. The project is then
        # removed by its name, and the reason says so.
        project(shop_block(worker=False, greeting='good morning'))
        run('run')
        remote(f'rm {SHOP}/web.env')
        project(shop_state('containers_removed_named_volumes_kept'))
        for command in ('dryrun', 'run'):
            by_name = results(run(command))['project']
            assert "Docker Compose cannot load the project's files (" in by_name['reason'] and 'web.env' in by_name['reason'], by_name
            assert by_name['reason'].endswith('so the project is removed by its name'), by_name
        assert services(by_name) == {'web': 'removed'} and container_ids(SHOP) == [] and remote('docker volume ls --quiet') == 'mmx-test-shop_web_data', by_name
        assert results(run('run'))['project']['change'] == 'unchanged'
        # The project is found without its Compose file, and the removal
        # names the volume that it deletes: the one that carries the
        # project's name as Compose's label.
        project(shop_block(worker=False, greeting='good morning'))
        run('run')
        remote(f'rm {SHOP}/compose.yaml')
        project(shop_state('containers_and_named_volumes_removed'))
        preview = results(run('dryrun'))['project']
        assert preview['reason'] == 'would delete the named volumes mmx-test-shop_web_data with the data in them', preview
        without_file = results(run('run'))['project']
        assert services(without_file) == {'web': 'removed'} and container_ids(SHOP) == [] and remote('docker volume ls --quiet') == '', without_file
        assert without_file['reason'] == 'deleted the named volumes mmx-test-shop_web_data with the data in them', without_file
        # A removal that is run again after its directory is gone reports
        # unchanged.
        remote(f'rm -r {SHOP}')
        for state in ('stopped', 'containers_removed_named_volumes_kept', 'containers_and_named_volumes_removed'):
            project(shop_state(state))
            assert results(run('run'))['project']['change'] == 'unchanged', state
        assert image_on_node(web_image['reference']) == image_id(web_image), 'a removal deleted an image'
        mark('state stopped keeps the containers and running starts the same ones; containers_removed_named_volumes_kept keeps the named volume, '
             'also when Compose cannot load the project\'s files; containers_and_named_volumes_removed deletes it and names it, also after the '
             'containers and without the Compose file; removals rerun unchanged')

        # A project with an image of the private registry, through
        # registry_logins.
        vault = '/opt/mmx-test-vault'
        vault_image = registry.publish('private/vault', 'stable', image_version='1')

        def vault_project(with_login=True):
            task = {'compose_project_name': 'mmx-test-vault', 'compose_project_dir': vault, 'state': 'running', 'pull_images': 'always', 'containers_of_removed_services': 'remove'}
            if with_login:
                task['registry_logins'] = [{'registry': REGISTRY, 'registry_username': image_registry.USERNAME, 'registry_password': '{{ .secrets.registry_password }}'}]
            return [{'directory': {'path': vault}},
                    {'file': {"permissions_mode": "0644", 'path': vault + '/compose.yaml', 'content': f'services:\n  vault:\n    image: {vault_image["reference"]}\n'}},
                    {'id': 'vault', 'docker_compose_project': task}]

        def vault_container():
            return remote("docker inspect --format '{{.Id}} {{.Image}} {{.State.Status}}' mmx-test-vault-vault-1 2>/dev/null || true")
        project(vault_project(with_login=False))
        refused = results(run('run', 1))['vault']
        assert refused['status'] == 'failed' and vault_container() == '', refused
        # The logins are handed only to docker, so they do not hide the
        # task's result: a wrong password shows the registry's reason, and
        # the run that starts the project lists its service and its image.
        project(vault_project(), **login)
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = 'not-the-password'
        wrong = results(run('run', 1))['vault']
        assert wrong['status'] == 'failed' and wrong.get('secret') is not True and (
            'docker_compose_project: docker compose ' in wrong['reason']) and wrong_password in wrong['reason'] and vault_container() == '', wrong
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = image_registry.PASSWORD
        registry.clear_requests()
        started_vault = results(run('run'))['vault']
        assert started_vault['change'] == 'changed' and started_vault.get('secret') is not True and services(started_vault) == {'vault': 'created'}, started_vault
        assert started_vault['value']['images'] == [{'image': vault_image['reference'], 'previous_image_id': None, 'image_id': image_id(vault_image)}], started_vault
        first_vault = vault_container()
        assert first_vault.split()[1:] == [image_id(vault_image), 'running'], first_vault
        assert [r for r in registry.requests() if r['authorized'] and r['kind'] == 'manifest' and r['status'] == 200], registry.requests()
        assert results(run('run'))['vault']['change'] == 'unchanged' and vault_container() == first_vault
        vault_image = registry.publish('private/vault', 'stable', image_version='2')
        registry.clear_requests()
        assert results(run('dryrun'))['vault']['change'] == 'would_change' and vault_container() == first_vault
        assert [r for r in registry.requests() if r['authorized'] and r['kind'] == 'manifest'], registry.requests()
        no_image_layer_was_fetched()
        assert results(run('run'))['vault']['change'] == 'changed'
        assert vault_container().split()[1:] == [image_id(vault_image), 'running'] and vault_container() != first_vault
        project([{'id': 'vault', 'docker_compose_project': {'compose_project_dir': vault, 'compose_project_name': 'mmx-test-vault',
                                                            'state': 'containers_and_named_volumes_removed'}}])
        assert services(results(run('run'))['vault']) == {'vault': 'removed'} and vault_container() == ''
        remote(f'rm -r {vault}')
        mark('a Compose project pulls an image of a private registry with registry_logins, in a dryrun asks with the login and pulls nothing, '
             'and follows the moved tag; its result lists the service and the image, and with a wrong password it gives the registry\'s reason')

        # The password of the registry login: on no process command line
        # while docker ran, in no mmx output, no saved run, no job record of
        # the node, not in its journal, and in no file that docker keeps.
        remote(f'touch {SAMPLES}/stop-sampler\nsleep 1')
        seen = remote(f'sort {SAMPLES}/command-lines | uniq -c')
        assert 'docker pull' in seen, 'the sampler never saw docker pull running'
        saved = ''.join(p.read_text(errors='replace') for root in (workspace, example) for p in (root / '.mmx').rglob('*') if p.is_file())
        for form in (image_registry.PASSWORD, stored_login):
            assert form not in seen, 'the registry password appeared on a process command line'
            assert form not in ''.join(printed) and form not in saved, 'the registry password left the controller process'
        found = remote(f'head -n 2 {SAMPLES}/patterns > {SAMPLES}/password\ntest "$(wc -l < {SAMPLES}/password)" = 2\n'
                       f'grep -rlF -f {SAMPLES}/password /var/lib/mmx /run/mmx /var/lib/docker /etc/docker /root /run/docker 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {SAMPLES}/password || true\n'
                       'ls -A /run/mmx/private_task_directories\nls -d /root/.docker 2>/dev/null || true')
        assert found == '', found
        # The logins were in the node tasks' private directories, and the
        # search above found the place where each one is made empty.
        assert remote('stat -c "%a %U" /run/mmx /run/mmx/private_task_directories') == '711 root\n711 root'
        mark('the registry password, plain and as docker stores a login, is on no command line, in no mmx output, saved run, node job record, '
             'or journal, and docker keeps no login')

        # The cleanup service of the example docker-host runs docker image
        # prune. With the classic image store it deletes the test images,
        # which no container uses and which were built long ago. With the
        # containerd image store the seven days count from the pull, so
        # images that came today stay.
        if docker_host_example:
            assert docker('image ls --quiet').split()
            remote('systemctl start docker-image-cleanup.service')
            assert remote('systemctl show docker-image-cleanup.service -p Result --value') == 'success'
            assert 'Total reclaimed space' in remote('journalctl --no-pager -q -u docker-image-cleanup.service')
            assert bool(docker('image ls --quiet')) == containerd_store, docker('image ls')
            mark('the example docker-host installs Docker from its own repository inside a version pattern with the daemon settings, '
                 'and its cleanup service runs docker image prune')

        # docker_unused_images. Each image is imported from a tar file of two
        # MiB of random bytes, so it has a layer of its own.
        def import_image(tag):
            return remote(f'head -c 2097152 /dev/urandom > {SAMPLES}/unused-image-data\n'
                          f'tar -C {SAMPLES} -c unused-image-data | docker image import - {tag}').split()[-1]

        def present(image_id):
            return remote(f'docker image inspect {image_id} >/dev/null 2>&1 && echo yes || true') == 'yes'

        def removed(result):
            return {i['image_id'] for i in result['value']['removed_images']}

        remote(f'mkdir -p {SAMPLES}')
        old = import_image('localhost/mmx-unused/app:1')
        new = import_image('localhost/mmx-unused/app:1')
        # The image of a stopped container stays, also once it has no name.
        kept = import_image('localhost/mmx-unused/kept:1')
        docker('create --name mmx-unused-keeper localhost/mmx-unused/kept:1 /unused-image-data')
        kept_named = import_image('localhost/mmx-unused/kept:1')
        used_ids = {remote(f"docker container inspect --format '{{{{.Image}}}}' {c}") for c in docker('container ls --all --quiet --no-trunc').split()}
        project([{'id': 'images', 'docker_unused_images': {'remove': 'dangling_images_only'}}])
        preview = results(run('dryrun'))['images']
        # The containerd image store also lists as dangling an image whose
        # dangling record stays beside records that give it names; prune
        # keeps such an image.
        dangling = {i for i in set(docker('image ls --filter dangling=true --quiet --no-trunc').split()) - used_ids
                    if docker(f"image inspect --format '{{{{len .RepoTags}}}}' {i}") == '0'}
        assert preview['change'] == 'would_change' and removed(preview) == dangling and old in dangling and kept not in removed(preview), (preview, dangling)
        assert preview['value']['freed_bytes'] is None and present(old), preview
        ran = results(run('run'))['images']
        assert ran['change'] == 'changed' and removed(ran) == dangling and ran['value']['freed_bytes'] >= 2000000, (
            ran, dangling, docker('image ls --all --no-trunc'))
        assert ran['reason'].startswith(f"removed {len(dangling)} images from the node's Docker images and freed "), ran
        assert not present(old) and present(new) and present(kept) and present(kept_named)
        for command in ('run', 'dryrun'):
            again = results(run(command))['images']
            assert again['change'] == 'unchanged' and again['value'] == {'removed_images': [], 'freed_bytes': None}, again
        # Every image that no container uses: the named ones go too; the
        # stopped container's stays. The containerd image store also keeps
        # the image that now has the name the container was created with.
        project([{'id': 'images', 'docker_unused_images': {'remove': 'every_image_no_container_uses'}}])
        preview = results(run('dryrun'))['images']
        assert preview['change'] == 'would_change' and new in removed(preview) and not removed(preview) & used_ids, (preview, used_ids)
        assert (kept_named in removed(preview)) != containerd_store, (preview, containerd_store)
        ran = results(run('run'))['images']
        assert ran['change'] == 'changed' and removed(ran) == removed(preview) and ran['value']['freed_bytes'] >= 2000000, (
            ran, preview, docker('image ls --all --no-trunc'))
        assert not present(new) and present(kept_named) == containerd_store and present(kept)
        assert set(docker('image ls --all --quiet --no-trunc').split()) <= used_ids | ({kept_named} if containerd_store else set()), (
            docker('image ls --all --no-trunc'), used_ids)
        assert unchanged(run('run'))
        docker('rm --force mmx-unused-keeper')
        docker(f'image rm --force {kept}')
        if containerd_store:
            docker(f'image rm --force {kept_named}')
        mark(f'docker_unused_images in the {"containerd" if containerd_store else "classic"} image store: a dryrun lists what docker image '
             'prune removes without the bytes, the run removes exactly those through the daemon and reports the bytes the daemon freed; '
             'every_image_no_container_uses also removes named images; the image of a stopped container stays, also without a name; '
             'reruns unchanged')
    finally:
        vm.ENV.pop('MMX_TEST_REGISTRY_PASSWORD', None)
        remote(f'touch {SAMPLES}/stop-sampler 2>/dev/null || true')
        registry.stop()
        added = sorted(set(remote(INSTALLED_PACKAGES).split()) - packages_before)
        remote(f'PACKAGES_THE_SUITE_ADDED={shlex.quote(" ".join(added))}\n' + REMOVE_DOCKER + f'sysctl -q -w net.ipv4.ip_forward={forwarding} || true\n')
    still_added = sorted(set(remote(INSTALLED_PACKAGES).split()) - packages_before)
    assert not still_added, still_added
    left = remote('command -v docker dockerd containerd 2>/dev/null || true\nip -brief link | grep -E "^(docker0|br-)" || true\n'
                  'for tool in iptables ip6tables; do command -v $tool-save >/dev/null 2>&1 && $tool-save 2>/dev/null | grep -i docker; done || true')
    assert left == '', left
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} Docker workflows passed in {time.monotonic() - started:.0f} s, of which installing Docker took {installed_after:.0f} s', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
