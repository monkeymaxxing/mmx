#!/usr/bin/env python3
"""podman_image, podman_quadlet, and wait_for's podman_container_is_healthy through the public CLI, checked with podman, systemctl, and the container's own HTTP answers."""
import base64
import json
import shlex
import shutil
import sys
import time

import image_registry
import vm

ACCOUNT = 'mmxshop'
SYSTEM_ACCOUNT = 'mmxshopsystem'
QUADLETS = '/etc/containers/systemd'
CONFIGURATION_DIRECTORY = '/etc/mmx-shop-web'
DROP_IN = '/etc/containers/registries.conf.d/50-mmx-test-image-registry.conf'
EXAMPLE_DROP_IN = '/etc/containers/registries.conf.d/50-image-registry.conf'
BASE = '/var/tmp/mmx-podman-containers'
HEALTH_COMMAND = '["/container-test-program","check-health"]'
# The distribution's own packages: Podman, and skopeo, with which a dryrun of
# pull_image: always asks the registry.
PACKAGES = ['podman', 'skopeo']
# On Debian and Ubuntu, Podman's package only recommends the programs that
# pods (catatonit) and the containers of an ordinary account (uidmap, a
# network program, dbus-user-session) need, and Podman's own network
# programs. A server whose apt installs no recommended packages lacks them:
# Hetzner's Debian images are such servers. So the suite names them, exactly
# as examples/podman-host does.
APT_PACKAGES_THAT_PODMAN_ONLY_RECOMMENDS = ['catatonit', 'uidmap', 'slirp4netns', 'dbus-user-session']
APT_PACKAGES_OF_PODMANS_NETWORK_PROGRAMS = ['netavark', 'aardvark-dns', 'nftables', 'passt']
SEMANAGE_PACKAGE = 'policycoreutils-python-utils'
# MMX's record of the volumes whose .volume file a removal took and kept.
KEPT_VOLUME_RECORDS = '/var/lib/mmx/podman_volumes_of_removed_quadlet_files'
# A service that systemd generates from an init script, on Debian and Ubuntu.
INIT_SCRIPT = 'mmx-init-script'
# The registry login as Podman stores it in a login file: base64 of user:password.
STORED_LOGIN = base64.b64encode(f'{image_registry.USERNAME}:{image_registry.PASSWORD}'.encode()).decode()


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'podman-containers'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}, 'vars': {'shop_web_volume': 'mmx-shop-web-data'}}, name='example')
    # The volume's Quadlet file comes from a template of the project.
    (workspace / 'files').mkdir()
    (workspace / 'files/mmx-shop-web-data.volume.tmpl').write_text('[Volume]\nVolumeName={{ .vars.shop_web_volume }}\n')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # What the last cleanup still found of a container network's firewall
    # rules, read while the firewall programs were installed.
    firewall_rules_left = ''
    # Everything mmx printed, to search for the registry password at the end.
    printed = []
    registry = image_registry.Registry(name=vm.NAME)

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def as_account(script):
        """Run script as the account, with its runtime directory, as its own systemd runs its services."""
        return remote(f'cd /\nrunuser -u {ACCOUNT} -- env XDG_RUNTIME_DIR=/run/user/$(id -u {ACCOUNT}) /bin/sh -c {shlex.quote("set -eu; " + script)}')

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='podman_containers', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **settings)

    def run(command, expected=None, timeout=300):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            # systemd's own message says only "see journalctl": show the
            # node's last journal lines, without those of the health checks
            # that run every second, so that the failure can be read here.
            journal = vm.guest('sudo', '-n', '/bin/sh', '-c',
                               'journalctl --no-pager --since -90s --output short | '
                               'grep -v -e "healthcheck run" -e start-limit-hit -e "repeated too quickly" -e ssh-generator | tail -n 60',
                               check=False).stdout
            raise AssertionError((command, result.stdout, result.stderr, 'the last journal lines of the node:\n' + journal))
        return json.loads(result.stdout)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def changes(record):
        return {name: r['change'] for name, r in results(record).items()}

    def overall(record):
        return record['entries'][0]['response']['change']

    def unchanged(record):
        """The record's overall change is unchanged; a failure shows what the record's tasks say."""
        assert overall(record) == 'unchanged', (overall(record), {name: (r['status'], r['change'], r.get('reason', '')) for name, r in results(record).items()})
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def answer(port=8080):
        """What the container's program says about itself."""
        return json.loads(remote(f'curl -sS --max-time 5 http://127.0.0.1:{port}/'))

    def image_id(reference, account=None):
        """The ID of the image in the store, as podman itself says it; '' when the store does not have it."""
        command = f"podman image inspect --format '{{{{.Id}}}}' {reference} 2>/dev/null || true"
        return as_account(command) if account else remote(command)

    def layers_fetched():
        return [r for r in registry.requests() if r['blob_kind'] == 'image_layer']

    def cleanup():
        # Containers, services, files, and data of the suite, then the
        # account, then Podman itself, so that the next suite finds the
        # guest as this one found it.
        remove_semanage = '' if had_semanage else f'if command -v dnf >/dev/null; then dnf remove -y -q {SEMANAGE_PACKAGE} >/dev/null 2>&1 || true; fi'
        apt_packages = ' '.join(PACKAGES + apt_packages_from_the_suite)
        nonlocal firewall_rules_left
        printed_by_cleanup = remote(f"""
if id {ACCOUNT} >/dev/null 2>&1; then
  uid=$(id -u {ACCOUNT})
  if [ -S /run/user/$uid/systemd/private ] && command -v podman >/dev/null; then
    runuser -u {ACCOUNT} -- env XDG_RUNTIME_DIR=/run/user/$uid /bin/sh -c 'cd /; systemctl --user stop mmx-shop-web.service 2>/dev/null; podman rm --all --force >/dev/null 2>&1; podman volume rm --all --force >/dev/null 2>&1; podman rmi --all --force >/dev/null 2>&1' || true
  fi
  loginctl disable-linger {ACCOUNT} 2>/dev/null || true
  systemctl stop user@$uid.service 2>/dev/null || true
  userdel --remove {ACCOUNT} 2>/dev/null || true
fi
userdel {SYSTEM_ACCOUNT} 2>/dev/null || true
for unit in mmx-shop-web mmx-shop-plain mmx-shop-web-data-volume mmx-shop-network mmx-shop-in-pod mmx-shop-pod shop-web shop-web-data-volume; do systemctl stop $unit.service 2>/dev/null || true; done
if [ -e /etc/init.d/{INIT_SCRIPT} ]; then update-rc.d -f {INIT_SCRIPT} remove >/dev/null 2>&1 || true; rm -f /etc/init.d/{INIT_SCRIPT}; fi
systemctl disable --now podman-image-cleanup.timer 2>/dev/null || true
rm -f {QUADLETS}/mmx-shop-* {QUADLETS}/shop-web* /etc/systemd/system/podman-image-cleanup.service /etc/systemd/system/podman-image-cleanup.timer
systemctl daemon-reload
if command -v podman >/dev/null; then
  podman pod rm --all --force >/dev/null 2>&1 || true
  podman rm --all --force >/dev/null 2>&1 || true
  podman volume rm --all --force >/dev/null 2>&1 || true
  podman network rm systemd-mmx-shop >/dev/null 2>&1 || true
  podman rmi --all --force >/dev/null 2>&1 || true
fi
rm -rf {CONFIGURATION_DIRECTORY} {BASE} {DROP_IN} {EXAMPLE_DROP_IN} /etc/shop-web /var/lib/shop-web-backups /usr/local/bin/podman {KEPT_VOLUME_RECORDS}
# Podman's network program, netavark, keeps its firewall table (nftables) or
# chains (iptables) when the last container is gone. They go now, while nft
# and iptables are still installed: removing Podman's packages can remove
# both programs, and the table would stay in the kernel where no program
# shows it.
if command -v nft >/dev/null; then nft delete table inet netavark 2>/dev/null || true; fi
for tool in iptables ip6tables; do
  if command -v $tool-save >/dev/null && $tool-save 2>/dev/null | grep -q -i netavark; then $tool-save | grep -v -i netavark | $tool-restore; fi
done
if command -v nft >/dev/null; then nft list ruleset 2>/dev/null | grep -i -E 'netavark|podman' | sed 's/^/FIREWALL nft: /' || true; fi
for tool in iptables ip6tables; do
  if command -v $tool-save >/dev/null; then $tool-save 2>/dev/null | grep -i -E 'netavark|podman' | sed "s/^/FIREWALL $tool: /" || true; fi
done
if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q --autoremove {apt_packages} >/dev/null 2>&1 || true
else dnf remove -y -q {' '.join(PACKAGES)} >/dev/null 2>&1 || true
fi
if command -v semanage >/dev/null; then for directory in {CONFIGURATION_DIRECTORY} /etc/shop-web; do semanage fcontext -d "$directory(/.*)?" 2>/dev/null || true; done; fi
{remove_semanage}
rm -rf /var/lib/containers /run/containers /run/libpod /etc/containers/systemd
systemctl reset-failed 2>/dev/null || true
""")
        firewall_rules_left = '\n'.join(line for line in printed_by_cleanup.split('\n') if line.startswith('FIREWALL '))

    def quadlet(image, greeting_environment='one', health=True, reload_command=False, name='mmx-shop-web', port=8080, account=None):
        """The Quadlet file of the test container. It keeps its data in a
        named volume and reads its configuration from a directory of the
        node."""
        lines = ['[Unit]', 'Description=MMX test container', '', '[Container]', f'ContainerName={name}', f'Image={image}',
                 f'Environment=PORT={port}', f'Environment=GREETING={greeting_environment}', f'PublishPort=127.0.0.1:{port}:{port}']
        if not account:
            lines += [f'Volume={name}-data.volume:/data', f'Volume={CONFIGURATION_DIRECTORY}:/etc/container-test-program:ro']
        if health:
            lines += [f'HealthCmd={HEALTH_COMMAND}', 'HealthInterval=1s', 'HealthStartPeriod=0', 'HealthRetries=2']
        lines += ['', '[Service]', 'Restart=always']
        if reload_command:
            lines.append(f'ExecReload=/usr/bin/podman exec {name} /container-test-program reload-configuration')
        lines += ['', '[Install]', 'WantedBy=' + ('default.target' if account else 'multi-user.target'), '']
        task = {'name': name + '.container', 'content': '\n'.join(lines)}
        if account:
            task['in_systemd_of_user_account'] = account
        return {'id': 'quadlet', 'podman_quadlet': task}

    def with_line(task, before, line):
        """The Quadlet task with one more line in its file, before the named section."""
        changed = json.loads(json.dumps(task))
        changed['podman_quadlet']['content'] = changed['podman_quadlet']['content'].replace(before, line + '\n\n' + before)
        return changed

    def no_private_task_directory_left():
        """No node task left its private directory behind: the place where
        each one is made, for a registry login or for Podman's check of a
        Quadlet file, exists and is empty."""
        remote('test -d /run/mmx/private_task_directories\ntest -z "$(ls -A /run/mmx/private_task_directories)"')

    def no_password(what):
        """The registry password is in nothing that mmx printed or saved, and
        nowhere on the node: not as it is, and not as Podman stores a login.
        Podman's own login files are among the places."""
        saved = ''.join(path.read_text(errors='replace') for path in (workspace / '.mmx').rglob('*') if path.is_file())
        for form in (image_registry.PASSWORD, STORED_LOGIN):
            assert form not in ''.join(printed) and form not in saved, what + ': the registry password left the node task'
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'cat > {BASE}/password')], input=image_registry.PASSWORD + '\n' + STORED_LOGIN + '\n')
        found = remote(f'grep -rlF -f {BASE}/password /var/lib/mmx /run/mmx /run/containers /run/user /root /home /etc/containers /var/lib/containers 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/password || true')
        assert found == '', what + ': ' + found
        no_private_task_directory_left()

    def private_image_for_the_account(private, login, secrets):
        """A pull with a registry login into the account's own image store.
        The account runs podman there, so the login file is the account's
        while the pull runs: a wrapper notes its owner and mode."""
        real = remote('command -v podman')
        remote(f"""install -m 666 /dev/null {BASE}/login-file-of-the-account
cat > /usr/local/bin/podman <<'WRAPPER'
#!/bin/sh
if [ "$1" = pull ]; then
  for argument; do case "$argument" in /run/mmx/private_task_directories/podman_image-*) stat -c '%U %a' "$argument" "${{argument%/*}}" > {BASE}/login-file-of-the-account;; esac; done
fi
exec {real} "$@"
WRAPPER
chmod 755 /usr/local/bin/podman""")
        project([{'id': 'image', 'podman_image': {'image': private['reference'], 'pull_image': 'always', 'in_image_store_of_user_account': ACCOUNT, **login}}], **secrets)
        registry.clear_requests()
        assert results(run('dryrun'))['image']['change'] == 'would_change' and not layers_fetched(), registry.requests()
        pulled = results(run('run'))['image']
        remote('rm -f /usr/local/bin/podman')
        # The login is handed only to Podman, so the result names the image.
        assert pulled['change'] == 'changed' and pulled.get('secret') is not True and pulled['value'] == {'image_id': private['image_id'], 'previous_image_id': None}, pulled
        assert 'sha256:' + image_id(private['reference'], account=ACCOUNT) == private['image_id']
        assert remote(f'cat {BASE}/login-file-of-the-account').split('\n') == [f'{ACCOUNT} 600', f'{ACCOUNT} 700']
        for command in ('run', 'dryrun'):
            assert results(run(command))['image']['change'] == 'unchanged'
        no_password('a private image for an account')
        mark('a private image for an ordinary account: the login file is the account\'s alone while the pull runs, and the password stays nowhere, '
             'also not in Podman\'s own login files')

    def deployment(image, pull_image='when_missing', configuration='first\n', refresh=None, validate=None, wait=True, **quadlet_settings):
        """The system's test container: its image, its configuration file,
        its two Quadlet files, its service, and the wait for its health."""
        service = {'name': 'mmx-shop-web.service', 'state': 'running'}
        if refresh:
            service['refresh'] = refresh
        block = {
            'id': 'shop_web',
            'changes': [
                {'id': 'image', 'podman_image': {'image': image, 'pull_image': pull_image}},
                {'id': 'configuration', 'file': {'path': CONFIGURATION_DIRECTORY + '/configuration', 'permissions_mode': '0644', 'content': configuration}},
                # The volume's file comes first: Podman's generator rejects a
                # container file that names a Quadlet file it cannot find.
                {'id': 'volume', 'podman_quadlet': {'name': 'mmx-shop-web-data.volume', 'template': 'files/mmx-shop-web-data.volume.tmpl'}},
                quadlet(image, **quadlet_settings),
            ],
            'activate': [{'id': 'service', 'systemd_service': service}],
        }
        if validate:
            # A failing validate stops the block after its changes and
            # before its activate phase, as a run that was interrupted there.
            block['validate'] = [{'id': 'validate', 'shell': {'script': validate}}]
        if wait:
            block['verify'] = [{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'wait_at_most': '30s'}}]
        return [
            # The container reads the directory; the label lets it, where the node has SELinux.
            {'selinux_file_context': {'path': CONFIGURATION_DIRECTORY, 'also_apply_to_everything_inside': True, 'selinux_type': 'container_file_t'}},
            {'directory': {'path': CONFIGURATION_DIRECTORY, 'permissions_mode': '0755'}},
            {'task_block': block},
        ]

    def example(name, variables):
        """Copy an example project beside the suite's own and give it the test node and variables."""
        directory = vm.ROOT / ('podman-example-' + name)
        if directory.exists():
            shutil.rmtree(directory)
        shutil.copytree(vm.REPO / 'examples' / name, directory)
        vm.write_inventory(directory, {'nodes': {'app': node}, 'vars': variables}, name='example')

        def play(command, playbook, expected=None):
            result = vm.cmd([mmx, command, playbook, '--json'], cwd=directory, check=False, timeout=600)
            printed.append(result.stdout + result.stderr)
            assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, playbook, result.stdout, result.stderr)
            return json.loads(result.stdout)
        return directory, play

    def host_example():
        """examples/podman-host on a node that has Podman: the registry
        drop-in, the cleanup timer of the system and of the account, and
        the account. Its drop-in takes the place of the suite's own."""
        remote(f'rm -f {DROP_IN}')
        _, play = example('podman-host', {'image_registry_location': image_registry.ADDRESS, 'image_registry_serves_plain_http': True,
                                         'container_account': ACCOUNT})
        applied = play('run', 'configure')
        assert remote(f'cat {EXAMPLE_DROP_IN}') == f'[[registry]]\nlocation = "{image_registry.ADDRESS}"\ninsecure = true'
        assert remote('systemctl is-enabled podman-image-cleanup.timer; systemctl is-active podman-image-cleanup.timer') == 'enabled\nactive'
        assert as_account('systemctl --user is-enabled podman-image-cleanup.timer; systemctl --user is-active podman-image-cleanup.timer') == 'enabled\nactive'
        remote(f'grep -q "^{ACCOUNT}:" /etc/subuid\ngrep -q "^{ACCOUNT}:" /etc/subgid\nsystemctl start podman-image-cleanup.service')
        as_account('systemctl --user start podman-image-cleanup.service')
        for command in ('run', 'dryrun'):
            assert unchanged(play(command, 'configure'))
        # Podman still reaches the test registry, now through the example's drop-in.
        project([{'id': 'image', 'podman_image': {'image': image, 'pull_image': 'always'}}])
        assert results(run('dryrun'))['image']['status'] == 'succeeded'
        mark('the example podman-host: Podman and skopeo, the registry drop-in, the image cleanup timer of the system and of an ordinary account, and an unchanged rerun')
        return applied

    def account_podman():
        """How the account's Podman runs its containers. On Debian and
        Ubuntu it hands them to the account's own systemd only with the
        package dbus-user-session, which Podman's package only recommends;
        without it every command warns and falls back to another way."""
        how = as_account("podman info --format '{{.Host.CgroupManager}} {{.Store.GraphDriverName}}'").split()
        print(f"The account's Podman: cgroup manager {how[0]}, storage driver {how[1]}", flush=True)
        assert not apt or how[0] == 'systemd', how
        # Debian 12's Podman 4.3 copies the whole image for every container
        # of the account, also on a default installation.
        debian_12 = distribution == 'debian' and version < (4, 4)
        assert not apt or how[1] == ('vfs' if debian_12 else 'overlay'), how

    def unused_images(image):
        """podman_unused_images in the system's store and in the account's. The images are built on the node FROM the test image, so
        each has a layer of its own, two MiB of random bytes, beside the test program's layer, which they share."""
        context = BASE + '/unused-images'
        remote(f'mkdir -p {context}\nchmod 755 {BASE} {context}\nprintf "FROM {image}\\nCOPY data /data\\n" > {context}/Containerfile')

        def build(tag, account=None):
            """A new image under tag; the image that had the tag keeps no name. Returns the new image's ID as MMX reports it."""
            remote(f'head -c 2097152 /dev/urandom > {context}/data\nchmod 644 {context}/data')
            command = f'podman build --quiet --tag {tag} --file {context}/Containerfile {context}'
            return 'sha256:' + (as_account(command) if account else remote(command)).split()[-1]

        def exists(image_id, account=None):
            command = f'podman image exists {image_id[7:]} && echo yes || true'
            return (as_account(command) if account else remote(command)) == 'yes'

        def removed(result):
            return {i['image_id'] for i in result['value']['removed_images']}

        old = build('localhost/mmx-unused/app:1')
        new = build('localhost/mmx-unused/app:1')
        # The image of a stopped container stays, also once it has no name.
        kept = build('localhost/mmx-unused/kept:1')
        remote('podman create --name mmx-unused-keeper localhost/mmx-unused/kept:1 /container-test-program >/dev/null\n'
               'podman untag localhost/mmx-unused/kept:1')
        size = {i['Id']: i['Size'] for i in json.loads(remote('podman images --all --format json'))}
        project([{'id': 'images', 'podman_unused_images': {'remove': 'dangling_images_only'}}])
        preview = results(run('dryrun'))['images']
        listed = json.loads(remote('podman images --all --filter dangling=true --filter containers=false --format json'))
        assert preview['change'] == 'would_change' and removed(preview) == {'sha256:' + i['Id'] for i in listed} and old in removed(preview), preview
        assert kept not in removed(preview) and new not in removed(preview), preview
        assert all(i['size_bytes'] == size[i['image_id'][7:]] and i['names'] == [] for i in preview['value']['removed_images']), preview
        # The old image's own layer takes its two MiB on disk; the test program's layer stays.
        assert preview['value']['freed_bytes'] >= 2097152, preview
        assert exists(old)
        ran = results(run('run'))['images']
        assert ran['change'] == 'changed' and removed(ran) == removed(preview) and ran['value']['freed_bytes'] == preview['value']['freed_bytes'], (ran, preview)
        assert ran['reason'].startswith(f"removed {len(removed(ran))} images from the system's image store and freed "), ran
        assert not exists(old) and exists(new) and exists(kept)
        for command in ('run', 'dryrun'):
            again = results(run(command))['images']
            assert again['change'] == 'unchanged' and again['value'] == {'removed_images': [], 'freed_bytes': None}, again
        # Every image that no container uses: the named one goes too, the
        # stopped container's stays, and with it the test program's layer.
        used = set(remote("podman ps --all --format '{{.ImageID}}'").split())
        project([{'id': 'images', 'podman_unused_images': {'remove': 'every_image_no_container_uses'}}])
        preview = results(run('dryrun'))['images']
        assert preview['change'] == 'would_change' and new in removed(preview) and kept not in removed(preview), preview
        assert not any(i[7:19] in {u[:12] for u in used} for i in removed(preview)), (preview, used)
        ran = results(run('run'))['images']
        assert ran['change'] == 'changed' and removed(ran) == removed(preview) and ran['value']['freed_bytes'] == preview['value']['freed_bytes'] >= 2097152, (ran, preview)
        assert not exists(new) and exists(kept)
        left = {i['Id'] for i in json.loads(remote('podman images --all --format json'))}
        assert all(i[:12] in {u[:12] for u in used} for i in left), (left, used)
        assert unchanged(run('run'))
        remote('podman rm --force mmx-unused-keeper >/dev/null\npodman rmi --force localhost/mmx-unused/kept:1 >/dev/null 2>&1 || true')
        mark('podman_unused_images: dangling_images_only lists in a dryrun what podman image prune removes and predicts the bytes of the layers no '
             'remaining image uses, and the run removes exactly those images and frees that; every_image_no_container_uses also removes named '
             'images; the image of a stopped container stays, also without a name; reruns unchanged')

        # The account's own store: what its Podman removes, as the account.
        # The example podman-host made the account.
        if remote(f'id -u {ACCOUNT} >/dev/null 2>&1 && echo yes || true') != 'yes':
            host_example()
        remote(f'chown -R {ACCOUNT} {context}')
        old = build('localhost/mmx-unused/app:1', account=ACCOUNT)
        new = build('localhost/mmx-unused/app:1', account=ACCOUNT)
        project([{'id': 'images', 'podman_unused_images': {'remove': 'dangling_images_only', 'in_image_store_of_user_account': ACCOUNT}}])
        preview = results(run('dryrun'))['images']
        assert preview['change'] == 'would_change' and old in removed(preview) and new not in removed(preview), preview
        driver = as_account("podman info --format '{{.Store.GraphDriverName}}'")
        # Podman names the layers of an image only with overlay.
        assert (preview['value']['freed_bytes'] is None) == (driver != 'overlay'), (driver, preview)
        ran = results(run('run'))['images']
        assert ran['change'] == 'changed' and removed(ran) == removed(preview) and ran['value']['freed_bytes'] == preview['value']['freed_bytes'], (ran, preview)
        assert ran['reason'].startswith(f'removed {len(removed(ran))} images from the image store of account {ACCOUNT}'), ran
        assert not exists(old, ACCOUNT) and exists(new, ACCOUNT)
        assert unchanged(run('run'))
        as_account('podman rmi --force localhost/mmx-unused/app:1 >/dev/null')
        mark(f'podman_unused_images with in_image_store_of_user_account removes the dangling images of the account\'s own store, as the account; '
             f'freed_bytes is known with the storage driver {driver}' + ('' if driver == 'overlay' else ' only after nothing: null'))

    def guest_is_as_found():
        """The next suite uses this guest, among them the firewall suites:
        nothing of Podman or of this suite is left, no network device and no
        firewall rule of a container network, and no unit of it keeps systemd
        from "running". Everything found is in the one failure."""
        left = remote(f"""
command -v podman skopeo || true
ip -o link show 2>/dev/null | grep -o -E 'podman[0-9]+|veth[0-9a-z]+' | sed 's/^/network device /' || true
if command -v nft >/dev/null; then nft list ruleset 2>/dev/null | grep -i -E 'netavark|podman' | sed 's/^/nft: /' || true; fi
if command -v iptables-save >/dev/null; then iptables-save 2>/dev/null | grep -i -E 'netavark|podman' | sed 's/^/iptables: /' || true; fi
id {ACCOUNT} 2>/dev/null || true
ls -d {QUADLETS}/mmx-shop-* {QUADLETS}/shop-web* {DROP_IN} {EXAMPLE_DROP_IN} /etc/shop-web {CONFIGURATION_DIRECTORY} /var/lib/shop-web-backups /var/lib/containers /run/mmx/private_task_directories/* /usr/local/bin/podman {KEPT_VOLUME_RECORDS} /etc/init.d/{INIT_SCRIPT} /etc/rc?.d/*{INIT_SCRIPT} 2>/dev/null || true
systemctl list-unit-files 'podman*' 'shop-web*' 'mmx-shop*' '{INIT_SCRIPT}*' --no-legend 2>/dev/null || true
""")
        left = '\n'.join(part for part in (firewall_rules_left, left) if part)
        state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', name=vm.NAME, check=False).stdout.strip()
        if state != 'running':
            left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, name=vm.NAME, check=False).stdout
        assert left == '', 'the suite left this on the guest:\n' + left
        print('The guest is as the suite found it: nothing of Podman is left, and systemd is running', flush=True)

    def installed_apt_packages():
        """Which of the packages that the suite names beside Podman and skopeo the node has."""
        names = ' '.join(APT_PACKAGES_THAT_PODMAN_ONLY_RECOMMENDS + APT_PACKAGES_OF_PODMANS_NETWORK_PROGRAMS)
        return set(remote("dpkg-query --show --showformat '${db:Status-Abbrev} ${Package}\\n' " + names + " 2>/dev/null | awk '$1 == \"ii\" {print $2}' || true").split())

    distribution = remote('. /etc/os-release; echo "$ID"')
    selinux = remote(f'if {vm.SELINUX_RUNS}; then echo enabled; fi') == 'enabled'
    had_semanage = remote('command -v semanage || true') != ''
    apt = distribution in ('debian', 'ubuntu')
    # The packages beside Podman and skopeo that the suite itself installed,
    # which its cleanup removes again.
    apt_packages_from_the_suite = []
    if apt:
        # Whether this node's apt installs recommended packages, and what
        # sets that: the answer to "why does a pod start on one Debian server
        # and not on another".
        print('apt on this node: ' + remote(r"""apt-config dump | grep -i -e recommends -e suggests | tr '\n' ' ' || true
echo
for file in $(grep -rl -i -e recommends -e suggests /etc/apt/apt.conf /etc/apt/apt.conf.d 2>/dev/null || true); do
  echo "set in $file, owned by $(dpkg -S "$file" 2>/dev/null | cut -d: -f1 || true), last changed $(stat -c %y "$file" | cut -c1-19): $(grep -i -e recommends -e suggests "$file" | tr '\n' ' ')"
done
echo "named by cloud-init in: $(grep -rl -i -e install-recommends -e install_recommends /etc/cloud /var/lib/cloud/instance/user-data.txt /var/lib/cloud/instance/vendor-data.txt 2>/dev/null | tr '\n' ' ' || true)"
echo "the image: $(cat /etc/cloud/build.info 2>/dev/null | tr '\n' ' ' || true)"
"""), flush=True)
    apt_packages_before = installed_apt_packages() if apt else set()
    cleanup()
    try:
        registry.start()
        one = registry.publish('mmx-test/app', 'stable', image_version='1')
        image = one['reference']

        # Before Podman is installed: every module names the missing package
        # in a run and is not checked in a dryrun.
        project([{'id': 'image', 'podman_image': {'image': image, 'pull_image': 'when_missing'}}])
        assert results(run('dryrun'))['image']['status'] == 'not_checked'
        refused = results(run('run', 1))['image']
        assert refused['failure_kind'] == 'missing_prerequisite' and refused['reason'] == 'podman_image: podman is not installed; install the package podman in an earlier node task', refused
        project([quadlet(image)])
        assert results(run('dryrun'))['quadlet']['status'] == 'not_checked'
        refused = results(run('run', 1))['quadlet']
        assert refused['failure_kind'] == 'missing_prerequisite' and 'podman is not installed; install the package podman in an earlier node task' in refused['reason'], refused
        project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'wait_at_most': '5s'}}])
        assert results(run('dryrun'))['healthy']['status'] == 'not_checked'
        refused = results(run('run', 1))['healthy']
        assert refused['failure_kind'] == 'missing_prerequisite' and 'podman is not installed; install the package podman in an earlier node task' in refused['reason'], refused
        mark('without Podman, podman_image, podman_quadlet, and the health wait say to install Podman; a dryrun is not checked')

        # Podman and skopeo from the distribution's own packages, and the
        # drop-in that lets them use the test registry over plain HTTP.
        on_apt = {'equals': ['{{ .node.package_manager }}', 'apt']}
        install = vm.package_tasks(PACKAGES) + [
            {'when': on_apt, 'apt_package': {'packages': [{'name': package_name, 'version': 'newest_when_first_installed'} for package_name in APT_PACKAGES_THAT_PODMAN_ONLY_RECOMMENDS]}},
            {'when': on_apt, 'apt_package': {'packages': [{'name': package_name, 'version': 'newest_when_first_installed'} for package_name in APT_PACKAGES_OF_PODMANS_NETWORK_PROGRAMS]}}]
        # The example names the same packages.
        example_packages = (vm.REPO / 'examples/podman-host/playbooks/configure.yml').read_text()
        for name in PACKAGES + APT_PACKAGES_THAT_PODMAN_ONLY_RECOMMENDS + APT_PACKAGES_OF_PODMANS_NETWORK_PROGRAMS:
            assert name in example_packages, f'examples/podman-host does not install {name}'
        if selinux and not had_semanage:
            # selinux_file_context, which labels the directory that the
            # container reads, changes the policy with semanage.
            install.append({'dnf_package': {'packages': [{'name': SEMANAGE_PACKAGE, 'version': 'newest_when_first_installed'}]}})
        project(install + [{'file': {'path': DROP_IN, 'permissions_mode': '0644',
                                     'content': f'[[registry]]\nlocation = "{image_registry.ADDRESS}"\ninsecure = true\n'}}])
        run('run', timeout=1200)
        if apt:
            apt_packages_from_the_suite[:] = sorted(installed_apt_packages() - apt_packages_before)
            print('Beside Podman and skopeo the suite installed: ' + (' '.join(apt_packages_from_the_suite) or 'nothing; the node had them'), flush=True)
        version = tuple(int(part) for part in remote("podman version --format '{{.Client.Version}}'").split('.')[:2])
        print(f'Podman {version[0]}.{version[1]} on {distribution}', flush=True)

        # One image for the system's store.
        project([{'id': 'image', 'podman_image': {'image': image, 'pull_image': 'when_missing'}}])
        registry.clear_requests()
        preview = results(run('dryrun'))['image']
        assert preview['change'] == 'would_change' and preview['value'] == {'image_id': None, 'previous_image_id': None}, preview
        assert image_id(image) == '' and not registry.requests(), registry.requests()
        pulled = results(run('run'))['image']
        assert pulled['change'] == 'changed' and pulled['value'] == {'image_id': one['image_id'], 'previous_image_id': None}, pulled
        assert 'sha256:' + image_id(image) == one['image_id']
        registry.clear_requests()
        for command in ('run', 'dryrun'):
            again = results(run(command))['image']
            assert again['change'] == 'unchanged' and again['value'] == {'image_id': one['image_id'], 'previous_image_id': one['image_id']}, again
        assert not registry.requests(), 'when_missing asked the registry for an image the node has'
        mark('podman_image pulls a missing image, reports its ID as podman has it, and with when_missing asks the registry nothing on a rerun')

        # The tag moves. when_missing stays; always follows: its dryrun asks
        # the registry without fetching a layer and reports both IDs.
        two = registry.publish('mmx-test/app', 'stable', image_version='2')
        assert two['image_id'] != one['image_id']
        assert results(run('run'))['image']['change'] == 'unchanged' and 'sha256:' + image_id(image) == one['image_id']
        project([{'id': 'image', 'podman_image': {'image': image, 'pull_image': 'always'}}])
        registry.clear_requests()
        preview = results(run('dryrun'))['image']
        assert preview['change'] == 'would_change' and preview['value'] == {'image_id': two['image_id'], 'previous_image_id': one['image_id']}, preview
        assert registry.requests() and not layers_fetched(), registry.requests()
        assert 'sha256:' + image_id(image) == one['image_id']
        followed = results(run('run'))['image']
        assert followed['change'] == 'changed' and followed['value'] == {'image_id': two['image_id'], 'previous_image_id': one['image_id']}, followed
        assert 'sha256:' + image_id(image) == two['image_id']
        for command in ('run', 'dryrun'):
            assert results(run(command))['image']['change'] == 'unchanged'
        # A registry that does not answer, here an address where nothing
        # listens: the dryrun is not checked, the run fails with podman's reason.
        project([{'id': 'image', 'podman_image': {'image': '127.0.0.1:5999/mmx-test/app:stable', 'pull_image': 'always'}}])
        unreachable = results(run('dryrun'))['image']
        assert unreachable['status'] == 'not_checked' and 'podman_image: the registry does not say which image 127.0.0.1:5999/mmx-test/app:stable names: ' in unreachable['reason'], unreachable
        assert unreachable['value'] == {'image_id': None, 'previous_image_id': None}, unreachable
        # When the store has the image, the dryrun that cannot ask names it:
        # MMX then predicts nothing for a service that follows the image.
        remote(f'podman tag {image} 127.0.0.1:5999/mmx-test/app:stable')
        unreachable = results(run('dryrun'))['image']
        assert unreachable['status'] == 'not_checked' and unreachable['value'] == {'image_id': None, 'previous_image_id': two['image_id']}, unreachable
        remote('podman untag 127.0.0.1:5999/mmx-test/app:stable')
        failed = results(run('run', 1))['image']
        assert failed['status'] == 'failed' and 'podman pull' in failed['reason'] and '127.0.0.1:5999' in failed['reason'], failed
        mark('a moved tag: when_missing keeps the image; always reports both IDs in a dryrun that fetches no layer, then pulls; '
             'a registry that does not answer leaves the dryrun not checked and fails the run')

        # A private repository. The login reaches podman in a private file
        # under /run; the password is on no command line and in no record.
        private = registry.publish('private/app', '1', image_version='1')
        login = {'registry_username': image_registry.USERNAME, 'registry_password': '{{ .secrets.registry_password }}'}
        secrets = {'secrets_from_env': {'registry_password': 'MMX_TEST_REGISTRY_PASSWORD'}}
        project([{'id': 'image', 'podman_image': {'image': private['reference'], 'pull_image': 'always'}}])
        refused = results(run('run', 1))['image']
        assert refused['status'] == 'failed' and 'podman pull' in refused['reason'], refused
        assert image_id(private['reference']) == ''
        # The password is a secret, and MMX hands the login only to Podman,
        # so the login does not hide the task's result. With a wrong
        # password, the failure names the command without the login file and
        # gives the registry's reason.
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = 'not-the-password'
        project([{'id': 'image', 'podman_image': {'image': private['reference'], 'pull_image': 'always', **login}}], **secrets)
        refused = results(run('run', 1))['image']
        assert refused['status'] == 'failed' and refused.get('secret') is not True and image_id(private['reference']) == '', refused
        # The last words are the registry's own.
        assert 'podman_image: podman pull --quiet ' + private['reference'] + ': ' in refused['reason'] and 'incorrect username or password' in refused['reason'], refused
        assert '--authfile' not in refused['reason'] and 'private_task_directories' not in refused['reason'], refused
        vm.ENV['MMX_TEST_REGISTRY_PASSWORD'] = image_registry.PASSWORD
        registry.clear_requests()
        preview = results(run('dryrun'))['image']
        assert preview['change'] == 'would_change' and preview.get('secret') is not True and preview['value'] == {'image_id': private['image_id'], 'previous_image_id': None}, preview
        assert any(r['authorized'] and r['kind'] == 'manifest' for r in registry.requests()) and not layers_fetched(), registry.requests()
        # While the pull runs, a sampler reads every process command line. A
        # wrapper keeps podman pull alive for a second, so that the sampler
        # sees it; the patterns go through standard input, since the sampler
        # would find them on a command line.
        real = remote('command -v podman')
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'mkdir -p {BASE} && cat > {BASE}/patterns')], input=image_registry.PASSWORD + '\n' + STORED_LOGIN + '\n--authfile\n')
        remote(f"""cat > /usr/local/bin/podman <<'WRAPPER'
#!/bin/sh
if [ "$1" = pull ]; then sleep 1; fi
exec {real} "$@"
WRAPPER
chmod 755 /usr/local/bin/podman
cat > {BASE}/sampler <<'SAMPLER'
while test ! -e {BASE}/stop-sampler; do
  cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\000' '\\n' | grep -a -F -f {BASE}/patterns >> {BASE}/command-lines
done
SAMPLER
touch {BASE}/command-lines
nohup sh {BASE}/sampler >/dev/null 2>&1 &""")
        registry.clear_requests()
        pulled = results(run('run'))['image']
        remote(f'touch {BASE}/stop-sampler\nsleep 1\nrm -f /usr/local/bin/podman')
        assert pulled['change'] == 'changed' and pulled.get('secret') is not True and pulled['value'] == {'image_id': private['image_id'], 'previous_image_id': None}, pulled
        assert 'sha256:' + image_id(private['reference']) == private['image_id']
        # The store already has the one layer that every test image shares,
        # so the pull fetches the private image's manifest and configuration.
        assert any(r['authorized'] and r['blob_kind'] == 'image_configuration' and '/private/app/' in r['path'] for r in registry.requests()), registry.requests()
        lines = remote(f'cat {BASE}/command-lines')
        assert '--authfile' in lines, 'the sampler never saw podman pull with its login file'
        assert image_registry.PASSWORD not in lines and STORED_LOGIN not in lines, 'the registry password appeared on a process command line'
        assert results(run('run'))['image']['change'] == 'unchanged'
        no_password('a private image')
        mark('a private repository: without the login and with a wrong password the pull fails, and the result gives the registry\'s reason; '
             'with the login from a secret the dryrun asks and the run pulls, and both results name the image; '
             'the password is on no command line and in no output, saved run, node record, journal, home directory, or login file of Podman, in either form, '
             'and its file under /run is gone')

        if version < (4, 4):
            # No Quadlet before Podman 4.4: the task names both versions.
            project([quadlet(image)])
            assert results(run('dryrun'))['quadlet']['status'] == 'not_checked'
            refused = results(run('run', 1))['quadlet']
            assert refused['failure_kind'] == 'missing_prerequisite', refused
            assert refused['reason'].startswith('podman_quadlet: Podman ') and ' is installed, and Quadlet files need Podman 4.4 or newer' in refused['reason'], refused
            remote(f'test ! -e {QUADLETS}/mmx-shop-web.container')
            mark(f'Podman {version[0]}.{version[1]} has no Quadlet: podman_quadlet names the installed and the needed version and installs nothing')
            # The health wait needs no Quadlet: a container that podman run
            # started, with and without a health check.
            remote(f"podman run --detach --name mmx-shop-web --health-cmd '{HEALTH_COMMAND}' --health-interval 1s {image} >/dev/null\n"
                   f'podman run --detach --name mmx-shop-plain {image} >/dev/null')
            project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'wait_at_most': '30s'}}])
            waited = results(run('run'))['healthy']
            assert waited['status'] == 'succeeded' and waited['reason'] == 'the container mmx-shop-web is healthy', waited
            project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-plain', 'wait_at_most': '60s'}}])
            refused = results(run('run', 1))['healthy']
            assert 'the container mmx-shop-plain declares no health check' in refused['reason'], refused
            mark(f'Podman {version[0]}.{version[1]}: the health wait reads a container that podman run started, and one without a health check fails at once')
            host_example()
            # The example made the account. Its own image store and the wait
            # on its container need no Quadlet either.
            project([{'id': 'image', 'podman_image': {'image': image, 'pull_image': 'when_missing', 'in_image_store_of_user_account': ACCOUNT}}])
            assert results(run('dryrun'))['image']['change'] == 'would_change' and image_id(image, account=ACCOUNT) == ''
            assert results(run('run'))['image']['change'] == 'changed' and 'sha256:' + image_id(image, account=ACCOUNT) == two['image_id']
            account_podman()
            for command in ('run', 'dryrun'):
                assert results(run(command))['image']['change'] == 'unchanged'
            as_account(f"podman run --detach --name mmx-shop-web --health-cmd '{HEALTH_COMMAND}' --health-interval 1s {image} >/dev/null")
            project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'podman_container_of_user_account': ACCOUNT, 'wait_at_most': '30s'}}])
            assert results(run('run'))['healthy']['reason'] == f'the container mmx-shop-web of account {ACCOUNT} is healthy'
            mark(f'Podman {version[0]}.{version[1]}: an ordinary account\'s image store and the health wait on its container')
            private_image_for_the_account(private, login, secrets)
            unused_images(image)
            (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
            print(f'{len(passed)} Podman workflows passed', flush=True)
            return

        # A container from a Quadlet file: first run, unchanged rerun.
        project(deployment(image))
        preview = run('dryrun')
        assert changes(preview)['quadlet'] == 'would_change' and changes(preview)['volume'] == 'would_change', changes(preview)
        assert results(preview)['quadlet']['value'] == {'generated_service': 'mmx-shop-web.service'}
        remote(f'test ! -e {QUADLETS}/mmx-shop-web.container\ntest ! -e {CONFIGURATION_DIRECTORY}\n! podman container exists mmx-shop-web')
        first = run('run')
        assert changes(first)['quadlet'] == 'changed' and changes(first)['service'] == 'changed' and results(first)['healthy']['status'] == 'succeeded', changes(first)
        assert results(first)['volume']['value'] == {'generated_service': 'mmx-shop-web-data-volume.service'}
        assert remote(f"stat -c '%U:%G %a' {QUADLETS}/mmx-shop-web.container {QUADLETS}/mmx-shop-web-data.volume").split('\n') == ['root:root 644', 'root:root 644']
        assert remote('systemctl is-active mmx-shop-web.service') == 'active' and remote('systemctl is-enabled mmx-shop-web.service') == 'generated'
        assert remote("podman inspect --format '{{.State.Health.Status}}' mmx-shop-web") == 'healthy'
        serving = answer()
        assert serving['image_version'] == '2' and serving['configuration'] == 'first\n', serving
        started = serving['process_started_at']
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        assert answer()['process_started_at'] == started
        mark('a Quadlet container: the dryrun writes nothing; the run pulls nothing new, installs both files, starts the generated service, '
             'and waits for the health check; the rerun is unchanged and keeps the process')

        def restarted_once(record, what):
            """The run restarted the container exactly once: a new process, and the rerun keeps it."""
            nonlocal started
            now = answer()['process_started_at']
            assert now != started, what + ': the container was not restarted'
            started = now
            assert unchanged(run('run')) and answer()['process_started_at'] == started, what + ': the rerun restarted the container again'

        # systemd cannot enable a generated service; the task names the
        # Quadlet file and says what to write instead.
        enabled_service = {'id': 'service', 'systemd_service': {'name': 'mmx-shop-web.service', 'state': 'running', 'start_at_boot': True}}
        project([enabled_service])
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['service']
            assert f'unit mmx-shop-web.service is generated from the Quadlet file {QUADLETS}/mmx-shop-web.container, so systemd can neither enable nor disable it; remove start_at_boot from this task' in refused['reason'], refused
            assert 'WantedBy=multi-user.target' in refused['reason'], refused
        assert answer()['process_started_at'] == started
        # With the Quadlet file's task in the same fleet stage, the project
        # does not load: before the first run the node has no such unit, and
        # no dryrun there could say it.
        project(deployment(image)[:-1] + [{'task_block': dict(deployment(image)[-1]['task_block'], activate=[enabled_service])}])
        for command in ('validate', 'dryrun', 'run'):
            refused = vm.cmd([mmx, command, *vm.playbook_args(command)], cwd=workspace, check=False)
            assert refused.returncode != 0, command
            assert 'unit mmx-shop-web.service is generated from the Quadlet file mmx-shop-web.container, so systemd can neither enable nor disable it; remove start_at_boot from this task' in refused.stdout + refused.stderr, (command, refused.stdout, refused.stderr)
            assert 'systemd_service.start_at_boot' in refused.stdout + refused.stderr, (command, refused.stdout, refused.stderr)
        assert answer()['process_started_at'] == started
        mark('enabled on a generated service fails with the Quadlet file\'s name and what to write instead, in dryrun and run, and touches nothing; '
             'beside the Quadlet file\'s own task the project does not load')

        if distribution in ('debian', 'ubuntu'):
            # The one generated unit that systemd can enable: the unit of an
            # init script. MMX manages enabled for it as for any unit.
            remote(f"""cat > /etc/init.d/{INIT_SCRIPT} <<'SCRIPT'
#!/bin/sh
### BEGIN INIT INFO
# Provides:          {INIT_SCRIPT}
# Required-Start:    $remote_fs
# Required-Stop:     $remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: MMX test of a service from an init script
### END INIT INFO
exit 0
SCRIPT
chmod 755 /etc/init.d/{INIT_SCRIPT}
systemctl daemon-reload""")
            assert remote(f'systemctl show -p UnitFileState --value {INIT_SCRIPT}.service') == 'generated'
            for wanted, state in ((True, 'enabled'), (False, 'disabled')):
                project([{'id': 'service', 'systemd_service': {'name': INIT_SCRIPT, 'state': 'do_not_start_or_stop', 'start_at_boot': wanted}}])
                assert changes(run('dryrun'))['service'] == 'would_change', state
                assert changes(run('run'))['service'] == 'changed', state
                assert remote(f'systemctl is-enabled {INIT_SCRIPT}.service 2>/dev/null || true') == state
                for command in ('run', 'dryrun'):
                    assert unchanged(run(command))
            remote(f'update-rc.d -f {INIT_SCRIPT} remove >/dev/null 2>&1 || true\nrm -f /etc/init.d/{INIT_SCRIPT}\nsystemctl daemon-reload')
            mark('a service that systemd generates from an init script is enabled and disabled as any unit, and reruns unchanged')

        # A Quadlet file that Podman's generator rejects is not installed.
        installed = remote(f'sha256sum {QUADLETS}/mmx-shop-web.container')

        def nothing_installed(what):
            assert remote(f'sha256sum {QUADLETS}/mmx-shop-web.container') == installed, what + ' replaced the installed file'
            # Nothing of the check stays in the directory or in the task's
            # private directory.
            listing = remote(f'ls -A {QUADLETS}')
            assert sorted(name for name in listing.split('\n') if name.startswith(('mmx-', '.'))) == ['mmx-shop-web-data.volume', 'mmx-shop-web.container'], listing
            no_private_task_directory_left()
            assert answer()['process_started_at'] == started and remote('systemctl show -p LoadState --value mmx-shop-web.service') == 'loaded'

        project([with_line(quadlet(image), '[Service]', 'NoSuchKey=1')])
        rejected = results(run('run', 1))['quadlet']
        assert rejected['failure_kind'] == 'validation' and "unsupported key 'NoSuchKey'" in rejected['reason'], rejected
        assert rejected['reason'].startswith("podman_quadlet: Podman's check of the new Quadlet file failed: the Quadlet generator of Podman "), rejected
        assert f'{QUADLETS}/mmx-shop-web.container' in rejected['reason'] and 'private_task_directories' not in rejected['reason'], rejected
        nothing_installed('the rejected file')
        # A file that the generator cannot read at all: a line that is no
        # section, key, or comment. The generator passes over such a file and
        # reads the next file of the same name, here the good installed one;
        # the new file must not pass on the strength of that.
        project([with_line(quadlet(image), '[Service]', 'this line is no key')])
        rejected = results(run('run', 1))['quadlet']
        assert rejected['failure_kind'] == 'validation' and 'the generator cannot read the file as a Quadlet file' in rejected['reason'], rejected
        assert 'private_task_directories' not in rejected['reason'] and 'mmx-stands-in' not in rejected['reason'], rejected
        nothing_installed('the unreadable file')
        mark('a Quadlet file that the generator rejects, or cannot read at all beside a good installed file of the same name, fails with the reason; '
             'the installed file and the container stay')

        # An unchanged file that systemd has not read yet, as after a run
        # that stopped before systemd's reload: the dryrun says that a run
        # would reload, the run reloads, and the container keeps its process.
        project(deployment(image))
        remote(f'touch {QUADLETS}/mmx-shop-web.container')
        assert remote('systemctl show -p NeedDaemonReload --value mmx-shop-web.service') == 'yes'
        pending = results(run('dryrun'))['quadlet']
        assert pending['change'] == 'would_change' and pending['reason'] == 'would reload systemd, which has not yet read the changed unit definitions', pending
        assert remote('systemctl show -p NeedDaemonReload --value mmx-shop-web.service') == 'yes'
        assert changes(run('run'))['quadlet'] == 'changed'
        assert remote('systemctl show -p NeedDaemonReload --value mmx-shop-web.service') == 'no' and answer()['process_started_at'] == started
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('an unchanged Quadlet file with a pending systemd reload: the dryrun reports the reload, the run makes it, and the container keeps its process')

        # What restarts a Podman container. A changed Quadlet file restarts,
        # also when the service task says refresh: reload.
        project(deployment(image, refresh='reload', greeting_environment='two'))
        preview = run('dryrun')
        assert changes(preview)['quadlet'] == 'would_change' and changes(preview)['service'] == 'would_change', changes(preview)
        assert results(preview)['service']['reason'] == 'service definition changed', results(preview)['service']
        assert answer()['process_started_at'] == started
        changed = run('run')
        assert changes(changed)['quadlet'] == 'changed' and results(changed)['service']['reason'] == 'service definition changed', results(changed)['service']
        restarted_once(changed, 'a changed Quadlet file')
        mark('a changed Quadlet file restarts its generated service once, also with refresh: reload')

        # A changed file that is mounted into the container: restart by default.
        project(deployment(image, greeting_environment='two', configuration='second\n'))
        changed = run('run')
        assert changes(changed)['configuration'] == 'changed' and changes(changed)['quadlet'] == 'unchanged' and changes(changed)['service'] == 'changed', changes(changed)
        assert answer()['configuration'] == 'second\n'
        restarted_once(changed, 'a changed mounted file')
        # With ExecReload= in the Quadlet file and refresh: reload, the
        # container reads the file again and keeps its process. Adding
        # ExecReload= is a changed definition: one restart.
        project(deployment(image, greeting_environment='two', configuration='second\n', refresh='reload', reload_command=True))
        restarted_once(run('run'), 'ExecReload= added to the Quadlet file')
        project(deployment(image, greeting_environment='two', configuration='third\n', refresh='reload', reload_command=True))
        changed = run('run')
        assert changes(changed)['configuration'] == 'changed' and changes(changed)['service'] == 'changed', changes(changed)
        reloaded = answer()
        assert reloaded['process_started_at'] == started and reloaded['configuration'] == 'third\n' and reloaded['configuration_reloads'] == 1, reloaded
        assert unchanged(run('run')) and answer()['configuration_reloads'] == 1
        mark('a changed mounted file restarts the container by default and, with ExecReload= and refresh: reload, is read again by the same process')

        # A changed image ID: the tag moves and pull_image: always follows it.
        three = registry.publish('mmx-test/app', 'stable', image_version='3')
        settings = dict(greeting_environment='two', configuration='third\n', refresh='reload', reload_command=True)
        project(deployment(image, pull_image='always', **settings))
        registry.clear_requests()
        preview = run('dryrun')
        assert results(preview)['image']['value'] == {'image_id': three['image_id'], 'previous_image_id': two['image_id']}, results(preview)['image']
        assert changes(preview)['service'] == 'would_change' and results(preview)['service']['reason'] == 'installed software version changed', results(preview)['service']
        assert not layers_fetched() and answer()['image_version'] == '2'
        changed = run('run')
        assert changes(changed)['image'] == 'changed' and changes(changed)['quadlet'] == 'unchanged' and changes(changed)['service'] == 'changed', changes(changed)
        assert answer()['image_version'] == '3'
        restarted_once(changed, 'a new image ID')
        mark('a moved tag with pull_image: always: the dryrun predicts the pull and the restart, the run pulls and restarts once with the new image, also with refresh: reload')

        # A run that stops between the change and the restart: the next run
        # finds the change in place and still restarts, once. validate fails
        # after the block's changes and before its activate phase.
        project(deployment(image, pull_image='always', validate='false', **dict(settings, greeting_environment='three')))
        run('run', 1)
        assert answer()['process_started_at'] == started and 'GREETING=three' in remote(f'cat {QUADLETS}/mmx-shop-web.container')
        project(deployment(image, pull_image='always', **dict(settings, greeting_environment='three')))
        recovered = run('run')
        assert changes(recovered)['quadlet'] == 'unchanged' and changes(recovered)['service'] == 'changed', changes(recovered)
        restarted_once(recovered, 'a Quadlet file changed by an interrupted run')
        four = registry.publish('mmx-test/app', 'stable', image_version='4')
        settings['greeting_environment'] = 'three'
        project(deployment(image, pull_image='always', validate='false', **settings))
        run('run', 1)
        assert 'sha256:' + image_id(image) == four['image_id'] and answer()['image_version'] == '3' and answer()['process_started_at'] == started
        project(deployment(image, pull_image='always', **settings))
        recovered = run('run')
        assert changes(recovered)['image'] == 'unchanged' and changes(recovered)['service'] == 'changed', changes(recovered)
        assert answer()['image_version'] == '4'
        restarted_once(recovered, 'an image pulled by an interrupted run')
        project(deployment(image, pull_image='always', validate='false', **dict(settings, configuration='fourth\n')))
        run('run', 1)
        assert answer()['configuration'] == 'third\n' and remote(f'cat {CONFIGURATION_DIRECTORY}/configuration') == 'fourth'
        project(deployment(image, pull_image='always', **dict(settings, configuration='fourth\n')))
        recovered = run('run')
        assert changes(recovered)['configuration'] == 'unchanged' and changes(recovered)['service'] == 'changed', changes(recovered)
        reloaded = answer()
        assert reloaded['configuration'] == 'fourth\n' and reloaded['process_started_at'] == started and reloaded['configuration_reloads'] == 1, reloaded
        assert unchanged(run('run'))
        settings['configuration'] = 'fourth\n'
        mark('after a run that stopped between the change and the restart, the next run still restarts once for a Quadlet file and for an image ID, '
             'and reloads once for a mounted file')

        # The health wait: an unhealthy container fails the wait with its
        # status and the health check's output; one without a health check
        # fails at once.
        data = remote("podman volume inspect --format '{{.Mountpoint}}' mmx-shop-web-data")
        # Podman calls the container unhealthy after HealthRetries failed checks; until then the wait is satisfied.
        remote(f'touch {data}/unhealthy\n'
               "for try in $(seq 1 40); do [ \"$(podman inspect --format '{{.State.Health.Status}}' mmx-shop-web)\" != unhealthy ] || exit 0; sleep 0.5; done\n"
               'echo "the container did not become unhealthy" >&2; exit 1')
        project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'wait_at_most': '6s'}}])
        unhealthy = results(run('run', 1))['healthy']
        assert 'wait_for ended after at most 6s' in unhealthy['reason'] and 'the container mmx-shop-web is unhealthy' in unhealthy['reason'], unhealthy
        assert 'its last health check exited with code 1' in unhealthy['reason'], unhealthy
        remote(f'rm {data}/unhealthy')
        assert results(run('run'))['healthy']['reason'] == 'the container mmx-shop-web is healthy'
        plain = quadlet(image, health=False, name='mmx-shop-plain', port=8081)
        plain['podman_quadlet']['content'] = '\n'.join(line for line in plain['podman_quadlet']['content'].split('\n') if not line.startswith('Volume='))
        project([{'task_block': {'id': 'plain', 'changes': [plain], 'activate': [{'id': 'service', 'systemd_service': {'name': 'mmx-shop-plain.service', 'state': 'running'}}]}},
                 {'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-plain', 'wait_at_most': '2m'}}])
        refused = results(run('run', 1))['healthy']
        assert refused['reason'] == ('the container mmx-shop-plain declares no health check, so Podman never reports it healthy; '
                                     'declare one with HealthCmd= in its Quadlet file, or wait for a port or a url instead'), refused
        assert answer(8081)['image_version'] == '4'
        project([{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-no-such-container', 'wait_at_most': '2s'}}])
        missing = results(run('run', 1))['healthy']
        assert 'podman has no container named mmx-no-such-container' in missing['reason'], missing
        mark('the health wait fails with the status and the last health check of an unhealthy container, at once for a container without a health check, '
             'and names a container that Podman does not have')

        # Stop the container for other work and start it again: the named volume keeps its data.
        starts = int(remote(f'wc -l < {data}/starts.log'))
        project([{'id': 'service', 'systemd_service': {'name': 'mmx-shop-web.service', 'state': 'stopped'}}])
        assert changes(run('run'))['service'] == 'changed'
        remote('! podman container exists mmx-shop-web\npodman volume exists mmx-shop-web-data')
        assert changes(run('run'))['service'] == 'unchanged'
        project([{'id': 'service', 'systemd_service': {'name': 'mmx-shop-web.service', 'state': 'running'}}])
        assert changes(run('run'))['service'] == 'changed'
        assert int(remote(f'wc -l < {data}/starts.log')) == starts + 1
        mark('systemd_service stops the container for other work and starts it again; its named volume keeps the data')

        # Removal. A volume in use cannot go with its data: Podman refuses,
        # and the Quadlet file stays for the next run.
        volume_removal = [{'id': 'volume', 'podman_quadlet': {'name': 'mmx-shop-web-data.volume', 'state': 'volume_and_its_data_removed'}}]
        project(volume_removal)
        in_use = results(run('run', 1))['volume']
        assert 'podman volume rm mmx-shop-web-data' in in_use['reason'] and 'being used' in in_use['reason'], in_use
        remote(f'test -f {QUADLETS}/mmx-shop-web-data.volume\npodman volume exists mmx-shop-web-data')
        # systemd stops the container together with the service of the volume
        # file that it names; a dryrun of that file's removal names both.
        project([{'id': 'volume', 'podman_quadlet': {'name': 'mmx-shop-web-data.volume', 'state': 'file_and_service_removed_named_volumes_kept'}}])
        preview = results(run('dryrun'))['volume']
        assert preview['reason'] == ('would stop mmx-shop-web-data-volume.service and with it mmx-shop-web.service, which systemd stops when mmx-shop-web-data-volume.service stops, '
                                     f'then delete {QUADLETS}/mmx-shop-web-data.volume, then reload systemd'), preview
        assert remote('systemctl is-active mmx-shop-web.service') == 'active'
        container_removal = [{'id': 'quadlet', 'podman_quadlet': {'name': 'mmx-shop-web.container', 'state': 'file_and_service_removed_named_volumes_kept'}},
                             {'id': 'plain', 'podman_quadlet': {'name': 'mmx-shop-plain.container', 'state': 'file_and_service_removed_named_volumes_kept'}}]
        project(container_removal)
        preview = results(run('dryrun'))['quadlet']
        assert preview['change'] == 'would_change' and preview['reason'] == f'would stop mmx-shop-web.service, then delete {QUADLETS}/mmx-shop-web.container, then reload systemd', preview
        assert remote('systemctl is-active mmx-shop-web.service') == 'active'
        removed = run('run')
        assert changes(removed) == {'quadlet': 'changed', 'plain': 'changed'}, changes(removed)
        assert results(removed)['quadlet']['reason'] == f'stopped mmx-shop-web.service, then deleted {QUADLETS}/mmx-shop-web.container', results(removed)['quadlet']
        assert remote('systemctl show -p LoadState --value mmx-shop-web.service') == 'not-found'
        remote(f'test ! -e {QUADLETS}/mmx-shop-web.container\n! podman container exists mmx-shop-web\npodman volume exists mmx-shop-web-data\ntest -s {data}/starts.log')
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        project(volume_removal)
        preview = results(run('dryrun'))['volume']
        assert preview['reason'].startswith('would delete the Podman volume mmx-shop-web-data with the data in it, then '), preview
        removed = results(run('run'))['volume']
        assert removed['change'] == 'changed' and removed['reason'].startswith('deleted the Podman volume mmx-shop-web-data with the data in it, then '), removed
        assert removed['reason'].endswith(f'deleted {QUADLETS}/mmx-shop-web-data.volume'), removed
        remote(f'! podman volume exists mmx-shop-web-data\ntest ! -e {QUADLETS}/mmx-shop-web-data.volume\ntest ! -e {data}')
        for command in ('run', 'dryrun'):
            again = run(command)
            assert unchanged(again)
            # The file is gone, so the task can look only for Podman's default name, and says so.
            assert results(again)['volume']['reason'] == (f"{QUADLETS}/mmx-shop-web-data.volume is gone, so MMX could check only Podman's default name for its volume, "
                                                          'and Podman has no volume named systemd-mmx-shop-web-data'), results(again)['volume']
        # A .network file's removal also removes its Podman network.
        project([{'task_block': {'id': 'shop_network', 'changes': [{'id': 'network', 'podman_quadlet': {'name': 'mmx-shop.network', 'content': '[Network]\n'}}],
                                 'activate': [{'systemd_service': {'name': 'mmx-shop-network.service', 'state': 'running'}}]}}])
        assert results(run('run'))['network']['value'] == {'generated_service': 'mmx-shop-network.service'}
        remote('podman network exists systemd-mmx-shop')
        project([{'id': 'network', 'podman_quadlet': {'name': 'mmx-shop.network', 'state': 'file_and_service_removed_named_volumes_kept'}}])
        removed = results(run('run'))['network']
        assert removed['change'] == 'changed' and removed['reason'].startswith('removed the Podman network systemd-mmx-shop, then '), removed
        remote(f'! podman network exists systemd-mmx-shop\ntest ! -e {QUADLETS}/mmx-shop.network')
        assert unchanged(run('run'))
        mark('removal: file_and_service_removed_named_volumes_kept stops the service and deletes the file and keeps the volume; volume_and_its_data_removed deletes the volume, '
             'and fails with Podman\'s reason while a container uses it; a .network file\'s removal removes its network; the reason names what was deleted and stopped, '
             'and in a dryrun every service that would stop; every rerun is unchanged')

        if version >= (5, 0):
            # A pod with one container, from a .pod file and a .container
            # file that names it. Starting the container's service starts
            # the pod; removing the pod's file stops both.
            pod_container = '\n'.join(['[Container]', 'ContainerName=mmx-shop-in-pod', f'Image={image}', 'Environment=PORT=8084', 'Pod=mmx-shop.pod',
                                       f'HealthCmd={HEALTH_COMMAND}', 'HealthInterval=1s', 'HealthStartPeriod=0', ''])
            project([{'task_block': {'id': 'shop_pod',
                                     'changes': [{'id': 'pod', 'podman_quadlet': {'name': 'mmx-shop.pod', 'content': '[Pod]\nPodName=mmx-shop\n'}},
                                                 {'id': 'quadlet', 'podman_quadlet': {'name': 'mmx-shop-in-pod.container', 'content': pod_container}}],
                                     'activate': [{'id': 'service', 'systemd_service': {'name': 'mmx-shop-in-pod.service', 'state': 'running'}}],
                                     'verify': [{'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-in-pod', 'wait_at_most': '30s'}}]}}])
            first = run('run')
            assert results(first)['pod']['value'] == {'generated_service': 'mmx-shop-pod.service'} and results(first)['healthy']['status'] == 'succeeded', changes(first)
            assert remote("podman ps --filter pod=mmx-shop --filter name=mmx-shop-in-pod --format '{{.Names}}'") == 'mmx-shop-in-pod'
            assert remote('systemctl is-active mmx-shop-pod.service mmx-shop-in-pod.service').split('\n') == ['active', 'active']
            for command in ('run', 'dryrun'):
                assert unchanged(run(command))
            project([{'id': 'pod', 'podman_quadlet': {'name': 'mmx-shop.pod', 'state': 'file_and_service_removed_named_volumes_kept'}},
                     {'id': 'quadlet', 'podman_quadlet': {'name': 'mmx-shop-in-pod.container', 'state': 'file_and_service_removed_named_volumes_kept'}}])
            preview = results(run('dryrun'))['pod']
            assert preview['reason'].startswith('would stop mmx-shop-pod.service and with it mmx-shop-in-pod.service, which systemd stops when mmx-shop-pod.service stops, then delete '), preview
            removed = run('run')
            assert changes(removed) == {'pod': 'changed', 'quadlet': 'changed'}, changes(removed)
            remote(f'! podman pod exists mmx-shop\n! podman container exists mmx-shop-in-pod\ntest ! -e {QUADLETS}/mmx-shop.pod\ntest ! -e {QUADLETS}/mmx-shop-in-pod.container')
            for command in ('run', 'dryrun'):
                assert unchanged(run(command))
            mark('a pod with one container from a .pod file and a .container file: the container\'s service starts the pod, '
                 'and the removal of the pod\'s file names and stops both services')

        # Containers of an ordinary account. What Podman needs of the account
        # is checked first, each with what to do; a dryrun is not checked,
        # because an earlier task can still provide it.
        home = '/home/' + ACCOUNT
        account_image = {'id': 'image', 'podman_image': {'image': image, 'pull_image': 'when_missing', 'in_image_store_of_user_account': ACCOUNT}}
        account_quadlet = quadlet(image, port=8082, account=ACCOUNT)
        account_service = {'id': 'service', 'systemd_service': {'name': 'mmx-shop-web.service', 'in_systemd_of_user_account': ACCOUNT, 'state': 'running'}}
        account_wait = {'id': 'healthy', 'wait_for': {'podman_container_is_healthy': 'mmx-shop-web', 'podman_container_of_user_account': ACCOUNT, 'wait_at_most': '30s'}}
        account_block = {'task_block': {'id': 'shop_web', 'changes': [account_image, account_quadlet], 'activate': [account_service], 'verify': [account_wait]}}
        project([account_image])
        assert results(run('dryrun'))['image']['status'] == 'not_checked'
        refused = results(run('run', 1))['image']
        assert refused['failure_kind'] == 'missing_prerequisite' and 'unknown user ' + ACCOUNT in refused['reason'], refused
        remote(f'useradd --system --no-create-home --home-dir /home/{SYSTEM_ACCOUNT} {SYSTEM_ACCOUNT}')
        system_quadlet = quadlet(image, port=8083, account=SYSTEM_ACCOUNT)
        project([system_quadlet])
        assert results(run('dryrun'))['quadlet']['status'] == 'not_checked'
        refused = results(run('run', 1))['quadlet']
        assert refused['failure_kind'] == 'missing_prerequisite' and f'account {SYSTEM_ACCOUNT} has no home directory at /home/{SYSTEM_ACCOUNT}' in refused['reason'], refused
        remote(f'install -d -o {SYSTEM_ACCOUNT} -g {SYSTEM_ACCOUNT} /home/{SYSTEM_ACCOUNT}')
        refused = results(run('run', 1))['quadlet']
        assert f'account {SYSTEM_ACCOUNT} has no range of sub-user IDs in /etc/subuid' in refused['reason'] and 'system: false' in refused['reason'], refused
        remote(f'! loginctl show-user {SYSTEM_ACCOUNT} 2>/dev/null | grep -qx Linger=yes\ntest ! -e /home/{SYSTEM_ACCOUNT}/.config\nrm -rf /home/{SYSTEM_ACCOUNT}\nuserdel {SYSTEM_ACCOUNT}')
        mark('an account that does not exist, has no home directory, or has no sub-ID ranges fails the task with which one; a dryrun is not checked; nothing is changed for it')

        create_account = {'id': 'account', 'user': {'name': ACCOUNT, 'system': False, 'home': home, 'create_home_directory': True}}
        # Lingering goes on only when the task can do its work. A Quadlet
        # file that Podman's check rejects leaves the new account without it.
        project([create_account, with_line(account_quadlet, '[Service]', 'NoSuchKey=1')])
        rejected = results(run('run', 1))['quadlet']
        assert rejected['failure_kind'] == 'validation' and "unsupported key 'NoSuchKey'" in rejected['reason'] and 'lingering' not in rejected['reason'], rejected
        remote(f'test ! -e /var/lib/systemd/linger/{ACCOUNT}\ntest ! -e {home}/.config/containers/systemd/mmx-shop-web.container')
        # podman_image needs the account's systemd before it can read the
        # account's image store. When its pull then fails, the reason says
        # that lingering was turned on.
        project([{'id': 'image', 'podman_image': {'image': '127.0.0.1:5999/mmx-test/app:stable', 'pull_image': 'when_missing', 'in_image_store_of_user_account': ACCOUNT}}])
        failed = results(run('run', 1))['image']
        # The suite's tasks try three times. The first try turned lingering
        # on and says so; the task's result keeps that try and the change.
        first_try = failed['tries'][0]
        assert 'podman pull' in first_try['reason'] and first_try['reason'].endswith(f'; before the failure, the task enabled lingering for account {ACCOUNT}, which stays on'), failed
        assert first_try['change'] == 'changed' and failed['change'] == 'changed' and 'podman pull' in failed['reason'], failed
        remote(f'test -e /var/lib/systemd/linger/{ACCOUNT}')
        remote(f'loginctl disable-linger {ACCOUNT}\nsystemctl stop user@$(id -u {ACCOUNT}).service\nuserdel --remove {ACCOUNT}')
        mark('lingering: a Quadlet file that the check rejects leaves the account without it, and a failed pull for the account says that it was turned on')

        # The whole deployment for an account that the same run creates.
        project([create_account, account_block])
        preview = run('dryrun')
        assert results(preview)['account']['change'] == 'would_change' and results(preview)['image']['status'] == 'not_checked', changes(preview)
        remote(f'! id {ACCOUNT} 2>/dev/null')
        first = run('run')
        assert changes(first)['image'] == 'changed' and changes(first)['quadlet'] == 'changed' and results(first)['healthy']['status'] == 'succeeded', changes(first)
        # The first task that needs the account's systemd turns lingering on and says so.
        assert results(first)['image']['reason'] == f'Enabled lingering for account {ACCOUNT}, so its services start at boot and keep running after logout', results(first)['image']
        assert 'lingering' not in results(first)['quadlet'].get('reason', '')
        assert remote(f'loginctl show-user {ACCOUNT} -p Linger --value') == 'yes'
        path = f'{home}/.config/containers/systemd/mmx-shop-web.container'
        assert remote(f"stat -c '%U:%G %a' {path}") == f'{ACCOUNT}:{ACCOUNT} 644'
        assert results(first)['quadlet']['value'] == {'generated_service': 'mmx-shop-web.service'}
        # The container is the account's: its store, its systemd, its process.
        assert 'sha256:' + image_id(image, account=ACCOUNT) == four['image_id']
        assert as_account('systemctl --user is-active mmx-shop-web.service') == 'active'
        assert as_account("podman inspect --format '{{.State.Health.Status}}' mmx-shop-web") == 'healthy'
        remote('! podman container exists mmx-shop-web')
        assert remote("ps -eo user:32,args | awk '$2 == \"/container-test-program\" {print $1}'") == ACCOUNT
        assert answer(8082)['image_version'] == '4'
        # Podman keeps the account's state in the account's runtime directory, not under /tmp.
        uid = remote(f'id -u {ACCOUNT}')
        assert as_account("podman info --format '{{.Store.RunRoot}}'").startswith(f'/run/user/{uid}/'), as_account("podman info --format '{{.Store.RunRoot}}'")
        account_podman()
        started = answer(8082)['process_started_at']
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        assert answer(8082)['process_started_at'] == started
        mark('an ordinary account that the same run creates: its image store, its Quadlet file in its home, its own systemd with lingering, '
             'and the health wait on its container; the rerun is unchanged')

        # The account runs the check of its own Quadlet files. A link in its
        # directory to a file that only root can read gives nothing of that
        # file away in the failure of a rejected file. Podman's generator
        # quotes a line that it cannot read in its message, so the file holds
        # such a line.
        root_only = remote(f"echo mmx-root-only-sentinel > {BASE}/root-only\nchmod 600 {BASE}/root-only\ncat {BASE}/root-only")
        remote(f'ln -s {BASE}/root-only {home}/.config/containers/systemd/mmx-shop-linked.container\nchown -h {ACCOUNT}:{ACCOUNT} {home}/.config/containers/systemd/mmx-shop-linked.container')
        project([with_line(quadlet(image, port=8082, account=ACCOUNT), '[Service]', 'NoSuchKey=1')])
        rejected = run('run', 1)
        assert "unsupported key 'NoSuchKey'" in results(rejected)['quadlet']['reason'], results(rejected)['quadlet']
        assert 'mmx-root-only-sentinel' not in json.dumps(rejected) and root_only == 'mmx-root-only-sentinel', results(rejected)['quadlet']
        assert answer(8082)['process_started_at'] == started
        no_private_task_directory_left()

        # The link goes before the next reload of the account's systemd:
        # Podman's own generator makes no service from a directory in which
        # it cannot open a file.
        remote(f'rm {home}/.config/containers/systemd/mmx-shop-linked.container')

        # A changed Quadlet file of the account restarts the account's service; removal stops it.
        project([create_account, {'task_block': {'id': 'shop_web', 'changes': [account_image, quadlet(image, port=8082, account=ACCOUNT, greeting_environment='two')],
                                                 'activate': [account_service], 'verify': [account_wait]}}])
        changed = run('run')
        assert changes(changed)['quadlet'] == 'changed' and changes(changed)['service'] == 'changed', changes(changed)
        assert answer(8082)['process_started_at'] != started
        project([{'id': 'quadlet', 'podman_quadlet': {'name': 'mmx-shop-web.container', 'in_systemd_of_user_account': ACCOUNT, 'state': 'file_and_service_removed_named_volumes_kept'}}])
        assert changes(run('run'))['quadlet'] == 'changed'
        remote(f'test ! -e {path}')
        assert as_account('systemctl --user show -p LoadState --value mmx-shop-web.service') == 'not-found'
        as_account('! podman container exists mmx-shop-web')
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('the account runs the check of its own Quadlet files, so a link there to a file of root gives nothing away; '
             'the account\'s changed Quadlet file restarts its service once, and its removal stops the service and deletes the file')
        private_image_for_the_account(private, login, secrets)

        # One service in two node task blocks, in the account's systemd. The
        # first block writes a file and refreshes the service where it runs;
        # on the first run the service does not exist yet. The second block
        # installs the Quadlet file and starts the service. The start loads
        # the file, so the rerun reloads nothing; a changed file reloads once.
        account_removal = [{'id': 'quadlet', 'podman_quadlet': {'name': 'mmx-shop-web.container', 'in_systemd_of_user_account': ACCOUNT, 'state': 'file_and_service_removed_named_volumes_kept'}}]

        def two_blocks(settings):
            refresh = {'name': 'mmx-shop-web.service', 'in_systemd_of_user_account': ACCOUNT, 'state': 'do_not_start_or_stop', 'refresh': 'reload'}
            return [create_account,
                    {'task_block': {'id': 'shop_web_settings',
                                    'changes': [{'id': 'settings', 'file': {'path': home + '/shop-web-settings', 'permissions_mode': '0644', 'content': settings}}],
                                    'activate': [{'id': 'refresh', 'systemd_service': refresh}]}},
                    {'task_block': {'id': 'shop_web', 'changes': [account_image, quadlet(image, port=8082, account=ACCOUNT, reload_command=True)],
                                    'activate': [account_service], 'verify': [account_wait]}}]

        project(two_blocks('first\n'))
        first = run('run')
        assert changes(first)['refresh'] == 'unchanged' and changes(first)['service'] == 'changed', changes(first)
        started = answer(8082)['process_started_at']
        assert unchanged(run('run'))
        project(two_blocks('second\n'))
        changed = run('run')
        assert changes(changed)['refresh'] == 'changed' and changes(changed)['service'] == 'unchanged', changes(changed)
        assert unchanged(run('run'))
        reloaded = answer(8082)
        assert reloaded['process_started_at'] == started and reloaded['configuration_reloads'] == 1, reloaded
        project(account_removal)
        assert changes(run('run'))['quadlet'] == 'changed'
        mark('one service of the account in two node task blocks: the later block\'s start loads the earlier block\'s file, the rerun reloads nothing, '
             'and a changed file reloads the same process once')

        host_example()

        # The example deploy-with-podman-quadlet, with the test image in the
        # place of the shop's. Two tags stand for two versions.
        version_one = registry.publish('mmx-test/shop', '1', image_version='shop 1')
        version_two = registry.publish('mmx-test/shop', '2', image_version='shop 2')
        variables = {'shop_web_image': version_one['reference'], 'shop_web_greeting': 'Welcome to the shop',
                     'shop_web_health_check_command': HEALTH_COMMAND, 'shop_web_health_check_interval': '1s',
                     'shop_web_reload_command': '/container-test-program reload-configuration',
                     'shop_web_config_dir_in_container': '/etc/container-test-program',
                     'shop_web_data_dir_in_container': '/data'}
        directory, play = example('deploy-with-podman-quadlet', variables)

        def deploy(command='run', **changed):
            variables.update(changed)
            vm.write_inventory(directory, {'nodes': {'app': node}, 'vars': variables}, name='example')
            return play(command, 'deploy')

        assert overall(deploy('dryrun')) == 'would_change'
        remote(f'test ! -e {QUADLETS}/shop-web.container\ntest ! -e /etc/shop-web')
        first = deploy()
        assert results(first)['healthy']['status'] == 'succeeded' and changes(first)['service'] == 'changed', changes(first)
        serving = answer()
        assert serving['image_version'] == 'shop 1' and serving['configuration'] == 'greeting = Welcome to the shop\n', serving
        started = serving['process_started_at']
        for command in ('run', 'dryrun'):
            assert unchanged(deploy(command))
        # A changed setting of the application: the same process reads its file again.
        changed = deploy(shop_web_greeting='Good evening')
        assert changes(changed)['configuration'] == 'changed' and changes(changed)['container'] == 'unchanged' and changes(changed)['service'] == 'changed', changes(changed)
        reloaded = answer()
        assert reloaded['configuration'] == 'greeting = Good evening\n' and reloaded['process_started_at'] == started and reloaded['configuration_reloads'] == 1, reloaded
        assert unchanged(deploy())
        # The next version: a new image and a changed Quadlet file, one restart.
        changed = deploy(shop_web_image=version_two['reference'])
        assert changes(changed)['image'] == 'changed' and changes(changed)['container'] == 'changed' and changes(changed)['service'] == 'changed', changes(changed)
        upgraded = answer()
        assert upgraded['image_version'] == 'shop 2' and upgraded['process_started_at'] != started and upgraded['configuration'] == 'greeting = Good evening\n', upgraded
        assert unchanged(deploy()) and answer()['process_started_at'] == upgraded['process_started_at']
        mark('the example deploy-with-podman-quadlet deploys, reruns unchanged, reloads the same process for a changed setting, and restarts once for the next image')

        # Stop for a backup and start again; remove and keep the data; remove with the data.
        data = remote("podman volume inspect --format '{{.Mountpoint}}' shop-web-data")
        starts = int(remote(f'wc -l < {data}/starts.log'))
        play('run', 'back_up')
        remote('test -s /var/lib/shop-web-backups/shop-web-data.tar\ntar -tf /var/lib/shop-web-backups/shop-web-data.tar | grep -q starts.log')
        assert answer()['image_version'] == 'shop 2' and int(remote(f'wc -l < {data}/starts.log')) == starts + 1
        record = f'{KEPT_VOLUME_RECORDS}/system/shop-web-data.volume'
        assert changes(play('run', 'remove_and_keep_data')) == {'container': 'changed', 'volume': 'changed'}
        remote(f'! podman container exists shop-web\npodman volume exists shop-web-data\ntest -s {data}/starts.log\ntest ! -e {QUADLETS}/shop-web.container\ntest ! -e {QUADLETS}/shop-web-data.volume')
        # The file that named the volume is gone; MMX keeps the name.
        assert remote(f"cat {record}\nstat -c '%U %a' {record}").split('\n') == ['shop-web-data', 'root 600']
        assert unchanged(play('run', 'remove_and_keep_data'))
        # Deploying again finds the kept data, and the installed file names
        # its volume again, so the record goes.
        deploy()
        assert int(remote(f'wc -l < {data}/starts.log')) == starts + 2
        remote(f'test ! -e {record}')
        removed = play('run', 'remove_with_data')
        assert changes(removed) == {'container': 'changed', 'volume': 'changed'}, changes(removed)
        remote(f'! podman container exists shop-web\n! podman volume exists shop-web-data\ntest ! -e {data}')
        for command in ('run', 'dryrun'):
            assert unchanged(play(command, 'remove_with_data'))
        mark('the example\'s other playbooks stop the container for a backup and start it again, remove it and keep its data, and remove it with its data')

        # The two removal playbooks one after the other: the data is kept
        # first and deleted in a later run, although the file that named the
        # volume went with the first.
        deploy()
        data = remote("podman volume inspect --format '{{.Mountpoint}}' shop-web-data")
        play('run', 'remove_and_keep_data')
        remote(f'podman volume exists shop-web-data\ntest -s {data}/starts.log\ntest ! -e {QUADLETS}/shop-web-data.volume\ntest -s {record}')
        preview = results(play('dryrun', 'remove_with_data'))['volume']
        assert preview['change'] == 'would_change' and preview['reason'] == 'would delete the Podman volume shop-web-data with the data in it, then reload systemd', preview
        remote(f'podman volume exists shop-web-data\ntest -s {record}')
        removed = play('run', 'remove_with_data')
        assert changes(removed) == {'container': 'unchanged', 'volume': 'changed'}, changes(removed)
        assert results(removed)['volume']['reason'] == 'deleted the Podman volume shop-web-data with the data in it', results(removed)['volume']
        remote(f'! podman volume exists shop-web-data\ntest ! -e {data}\ntest ! -e {record}')
        for command in ('run', 'dryrun'):
            assert unchanged(play(command, 'remove_with_data'))
        mark('the example\'s remove_and_keep_data and then remove_with_data: the second still deletes the volume that the first kept, by MMX\'s record of its name')
        unused_images(image)
    finally:
        # Also on the early ways out above, for a guest without Podman or
        # without Quadlet; a failure keeps its own message.
        failing = sys.exc_info()[0] is not None
        vm.ENV.pop('MMX_TEST_REGISTRY_PASSWORD', None)
        try:
            cleanup()
        finally:
            registry.stop()
        if not failing:
            guest_is_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} Podman workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
