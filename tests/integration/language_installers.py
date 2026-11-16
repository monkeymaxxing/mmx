#!/usr/bin/env python3
"""pip_package, gem_package, npm_global_package, and the five lock file modules through the public CLI, checked with the installers themselves."""
import json
import shlex
import shutil

import vm

BASE = '/srv/mmx-installers'
ACCOUNT = 'mmxapp'
HOME = BASE + '/home'
COMPOSER = '/usr/local/bin/composer'
COMPOSER_URL = 'https://getcomposer.org/download/2.8.4/composer.phar'
COMPOSER_SHA256 = 'c4c4e2e1beab0ea04e0bd042a5dbba9feda1fbf5eda0d36203958edd343c0a8a'
# Node.js from nodejs.org, unpacked under /opt as a project would, which the
# npm and Yarn tasks reach through their program paths.
NODE = '/opt/node-v22.23.3'
NPM, YARN = NODE + '/bin/npm', NODE + '/bin/yarn'
NODE_ARCHIVES = {
    'amd64': ('https://nodejs.org/dist/v22.23.3/node-v22.23.3-linux-x64.tar.xz', 'df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de'),
    'arm64': ('https://nodejs.org/dist/v22.23.3/node-v22.23.3-linux-arm64.tar.xz', 'a44aeb94849a299b22df10b9e622ec2f605c2183501bc40590705131de7c740f'),
}

# uv from its release archive, as the recipe in docs/how-to/install-languages.md
# for a language version that no package offers installs it, and a Python that uv installs.
UV_DIRECTORY = '/opt/uv-0.13.0'
UV = UV_DIRECTORY + '/uv'
UV_ARCHIVES = {
    'amd64': ('https://github.com/astral-sh/uv/releases/download/0.13.0/uv-x86_64-unknown-linux-gnu.tar.gz', '1468ebd5a5541121837c5a2817b9972ba6090fa6caa3d142620850a47fb75154'),
    'arm64': ('https://github.com/astral-sh/uv/releases/download/0.13.0/uv-aarch64-unknown-linux-gnu.tar.gz', '3ccfb6af6e242433eb552f7d9676abd5412c6595c497e990d9c8cb7b5bd4d2c3'),
}
UV_PYTHON_DIRECTORY = '/opt/mmx-uv-python'
UV_PYTHON_VERSION = '3.14.8'
PYPROJECT = ('[project]\nname = "uv-app"\nversion = "1.0.0"\nrequires-python = ">=3.9"\ndependencies = ["six==1.16.0"]\n\n'
             '[dependency-groups]\ndev = ["iniconfig==2.0.0"]\n')

# The language runtimes and installers each package manager offers. Node.js
# and Composer come from their publishers, the same on every distribution.
PACKAGES = {
    'apt': ['git', 'python3-venv', 'ruby', 'ruby-bundler', 'php-cli'],
    'dnf': ['git', 'python3', 'ruby', 'rubygems', 'rubygem-bundler', 'php-cli'],
}

PACKAGE_JSON = {'name': 'web', 'version': '1.0.0', 'private': True,
                'dependencies': {'ms': '2.1.3'}, 'devDependencies': {'is-number': '7.0.0'}}
GEMFILE = 'source "https://rubygems.org"\ngem "rack", "3.1.8"\ngroup :development do\n  gem "rake", "13.2.1"\nend\n'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'language-installers'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def as_app(directory, script):
        """Run script as the application's account, as a developer would to make a lock file."""
        return remote(f'cd {directory}\nrunuser -u {ACCOUNT} -- env HOME={HOME} PATH={NODE}/bin:/usr/local/bin:/usr/bin:/bin /bin/sh -c {shlex.quote("set -eu; " + script)}')

    def project(tasks):
        vm.write_project(workspace, project_name='language_installers', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def run(command, expected=None, timeout=300):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=timeout)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def task(record, name):
        return results(record)[name]

    def unchanged_rerun(name):
        assert task(run('run'), name)['change'] == 'unchanged'

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def cleanup():
        remote(f'rm -rf {BASE} {COMPOSER}\n'
               'gem uninstall rack --all --executables >/dev/null 2>&1 || true\n'
               f'rm -rf {NODE} {UV_DIRECTORY} {UV_PYTHON_DIRECTORY}\n'
               f'userdel {ACCOUNT} 2>/dev/null || true\ngroupdel {ACCOUNT} 2>/dev/null || true')

    package_manager = remote('for tool in apt-get dnf; do command -v $tool >/dev/null && { echo $tool; break; }; done')
    package_manager = {'apt-get': 'apt'}.get(package_manager, package_manager)
    account = [
        {'group': {'name': ACCOUNT, 'system': True}},
        {'user': {'name': ACCOUNT, 'primary_group': ACCOUNT, 'system': True, 'home': HOME, 'create_home_directory': True, 'shell': '/bin/sh'}},
        {'directory': {'path': BASE, 'owner': ACCOUNT, 'group': ACCOUNT}},
    ]
    cleanup()
    try:
        project(account + [
            *vm.package_tasks(PACKAGES[package_manager]),
            {'download': {'url': COMPOSER_URL, 'sha256': COMPOSER_SHA256, 'path': COMPOSER, 'permissions_mode': '0755'}},
            *[{'when': {'equals': ['{{ .node.arch }}', arch]},
               'unpack_archive': {'url': url, 'sha256': sha256, 'path': NODE, 'unpack_without_wrapping_folder': True}}
              for arch, (url, sha256) in NODE_ARCHIVES.items()],
            *[{'when': {'equals': ['{{ .node.arch }}', arch]},
               'unpack_archive': {'url': url, 'sha256': sha256, 'path': UV_DIRECTORY, 'unpack_without_wrapping_folder': True}}
              for arch, (url, sha256) in UV_ARCHIVES.items()],
        ])
        # The distributions' language packages take minutes to install.
        run('run', timeout=1200)

        # pip_package: a virtual environment the task creates, as the
        # application's account.
        venv = BASE + '/venv'

        def pip(**fields):
            return account + [{'id': 'pip', 'pip_package': {'virtual_environment': venv, 'run_as_user_account': ACCOUNT, **fields}}]

        project(pip(packages=[{'name': 'six', 'version': '==1.16.0'}]))
        preview = task(run('dryrun'), 'pip')
        assert preview['change'] == 'would_change' and 'would create the virtual environment' in preview['reason'], preview
        remote(f'test ! -e {venv}')
        installed = task(run('run'), 'pip')
        assert installed['change'] == 'changed' and installed['value'] == [{'name': 'six', 'from': None, 'to': '1.16.0'}], installed
        assert as_app(BASE, f'{venv}/bin/python -c "import six; print(six.__version__)"') == '1.16.0'
        assert remote(f'stat -c %U {venv}/bin/python') == ACCOUNT
        unchanged_rerun('pip')
        checked = task(run('dryrun'), 'pip')
        pip_version = as_app(BASE, f'{venv}/bin/python -m pip --version').split()[1]
        assert checked['change'] == 'unchanged' or (checked['status'] == 'not_checked' and 'needs pip 22.2' in checked['reason']), (pip_version, checked)
        remote(f'printf "six==1.17.0\\n" > {BASE}/requirements.txt')
        project(pip(requirements_file_on_node=BASE + '/requirements.txt'))
        assert task(run('run'), 'pip')['value'] == [{'name': 'six', 'from': '1.16.0', 'to': '1.17.0'}]
        project(pip(packages=[{'name': 'six'}], state='absent'))
        assert task(run('run'), 'pip')['value'] == [{'name': 'six', 'from': '1.17.0', 'to': None}]
        unchanged_rerun('pip')
        mark(f'pip_package creates a virtual environment as the account, installs, follows a requirements file, uninstalls, and reruns unchanged (pip {pip_version})')

        # What pip installed is a tracked input: a new version runs the
        # command that follows it once.
        def app(requirement):
            return account + [{'task_block': {
                'id': 'myapp',
                'changes': [{'pip_package': {'packages': [{'name': 'six', 'version': requirement.removeprefix('six')}], 'virtual_environment': venv, 'run_as_user_account': ACCOUNT}}],
                'activate': [{'id': 'restart_myapp', 'shell': {'script': f'echo restart >> {BASE}/restarts', 'only_when_tracked_inputs_changed': True}}],
            }}]

        def restarts():
            return remote(f'cat {BASE}/restarts 2>/dev/null | wc -l')

        project(app('six==1.16.0'))
        run('run')
        assert restarts() == '1'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and restarts() == '1'
        project(app('six==1.17.0'))
        planned = task(run('dryrun'), 'restart_myapp')
        assert planned['change'] == 'would_change' and planned['reason'] == f'would run: tracked input installation of pip_package in {venv} would change', planned
        run('run')
        assert restarts() == '2'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and restarts() == '2'
        mark('what pip_package installed is a tracked input: a new version runs the command that follows it once, and dryrun says so')

        # gem_package: an exact version, kept beside others; absent with a
        # version removes only that one.
        project([{'id': 'gem', 'gem_package': {'packages': [{'name': 'rack', 'version': '3.1.8'}], 'run_as_user_account': 'root'}}])
        gem = task(run('run'), 'gem')
        assert gem['change'] == 'changed' and gem['value'] == [{'name': 'rack', 'from': None, 'to': '3.1.8'}], gem
        assert '3.1.8' in remote('gem list --local --exact rack')
        unchanged_rerun('gem')
        assert task(run('dryrun'), 'gem')['change'] == 'unchanged'
        project([{'id': 'gem', 'gem_package': {'packages': [{'name': 'rack', 'version': '3.1.8'}], 'run_as_user_account': 'root', 'state': 'absent'}}])
        assert task(run('run'), 'gem')['change'] == 'changed'
        assert '3.1.8' not in remote('gem list --local --exact rack')
        unchanged_rerun('gem')
        mark('gem_package installs an exact version, reruns unchanged, and removes that version')

        # npm_global_package: Yarn 1 for the whole node, into the global
        # directory of the Node.js under /opt, which the Yarn project below
        # uses.
        project([{'id': 'yarn', 'npm_global_package': {'packages': [{'name': 'yarn', 'version': '1.22.22'}], 'run_as_user_account': 'root', 'npm_program_path': NPM}}])
        assert task(run('dryrun'), 'yarn')['value'] == [{'name': 'yarn', 'from': None, 'to': '1.22.22'}]
        assert task(run('run'), 'yarn')['change'] == 'changed'
        assert remote(f'PATH={NODE}/bin:$PATH {YARN} --version') == '1.22.22'
        unchanged_rerun('yarn')
        mark('npm_global_package installs an exact version with an npm under /opt, which finds the node beside it, and reruns unchanged')

        # The lock file modules. Each application directory belongs to the
        # account and carries a lock file, as a checkout of its repository
        # would.
        def application(name, files):
            directory = f'{BASE}/{name}'
            remote(f'mkdir -p {directory}\n' + ''.join(f'cat > {directory}/{file} <<\'EOF\'\n{text}\nEOF\n' for file, text in files.items())
                   + f'chown -R {ACCOUNT}:{ACCOUNT} {directory}')
            return directory

        def lock_file_module(module, directory, check, **fields):
            fields = {'lock_file_dir': directory, 'run_as_user_account': ACCOUNT, **fields}
            project([{'id': 'dependencies', module: fields}])
            preview = task(run('dryrun'), 'dependencies')
            assert preview['change'] == 'would_change' and preview['reason'] == 'would install: there is no record of an earlier installation', preview
            installed = task(run('run'), 'dependencies')
            assert installed['change'] == 'changed', installed
            check()
            unchanged_rerun('dependencies')
            assert task(run('dryrun'), 'dependencies')['change'] == 'unchanged'
            record = json.loads(remote(f'cat {directory}/.mmx-{module.replace("_", "-")}'))
            assert remote(f'stat -c %U {directory}/.mmx-{module.replace("_", "-")}') == ACCOUNT
            return installed, record

        web = application('web', {'package.json': json.dumps(PACKAGE_JSON)})
        as_app(web, 'npm install --package-lock-only --no-fund --no-audit >/dev/null')

        def only_production(directory, installed):
            def check():
                remote(f'test -f {directory}/{installed}/ms/package.json\ntest ! -e {directory}/{installed}/is-number')
                assert remote(f'find {directory}/{installed} ! -user {ACCOUNT} | wc -l') == '0'
            return check

        _, record = lock_file_module('npm_project_dependencies', web, only_production(web, 'node_modules'), include_development_dependencies=False, npm_program_path=NPM)
        assert record['lock_file'] == 'package-lock.json' and record['settings'] == {'include_development_dependencies': False}, record
        project([{'id': 'dependencies', 'npm_project_dependencies': {'lock_file_dir': web, 'run_as_user_account': ACCOUNT, 'include_development_dependencies': True, 'npm_program_path': NPM}}])
        assert task(run('dryrun'), 'dependencies')['reason'] == "would install: the task's settings changed since the last installation"
        run('run')
        remote(f'test -f {web}/node_modules/is-number/package.json')
        mark('npm_project_dependencies installs what package-lock.json lists without devDependencies, records it, reruns unchanged, and installs again for a changed setting')

        yarn_web = application('yarn-web', {'package.json': json.dumps(PACKAGE_JSON)})
        as_app(yarn_web, 'yarn install --non-interactive --no-progress >/dev/null && rm -rf node_modules')
        lock_file_module('yarn_project_dependencies', yarn_web, only_production(yarn_web, 'node_modules'), include_development_dependencies=False, yarn_program_path=YARN)
        mark('yarn_project_dependencies installs what yarn.lock lists with Yarn 1 without devDependencies and reruns unchanged')

        ruby_app = application('ruby-app', {'Gemfile': GEMFILE})
        as_app(ruby_app, 'bundle lock >/dev/null')
        bundle_files = f'find {ruby_app}/.bundle -type f -exec sh -c \'echo "== $1"; cat "$1"\' _ {{}} \\; 2>/dev/null || true'
        made_by_bundle_lock = remote(bundle_files)

        def gems_installed():
            remote(f'ls -d {ruby_app}/vendor/bundle/ruby/*/gems/rack-3.1.8\n'
                   f'test -z "$(ls -d {ruby_app}/vendor/bundle/ruby/*/gems/rake-13.2.1 2>/dev/null)"')

        bundled, record = lock_file_module('bundler_project_dependencies', ruby_app, gems_installed, without_gemfile_groups=['development'])
        assert bundled['value']['bundler_settings'] == {'BUNDLE_DEPLOYMENT': 'true', 'BUNDLE_PATH': 'vendor/bundle', 'BUNDLE_WITHOUT': 'development'}, bundled
        # Bundler before 2.3, such as Rocky Linux 9's 2.2, writes the groups
        # it left out into .bundle/config itself, even from the environment.
        after = remote(bundle_files)
        assert after == made_by_bundle_lock or after == f'== {ruby_app}/.bundle/config\n---\nBUNDLE_WITHOUT: "development"', (
            'a Bundler configuration file appeared', made_by_bundle_lock, after)
        # The application runs with the same settings.
        as_app(ruby_app, 'BUNDLE_DEPLOYMENT=true BUNDLE_PATH=vendor/bundle BUNDLE_WITHOUT=development bundle exec ruby -e "require %q(rack); puts Rack.release" >/dev/null')
        mark('bundler_project_dependencies installs what Gemfile.lock lists in deployment mode without a group, writes no Bundler file, and reruns unchanged')

        # Composer: two packages from a path repository, so the lock file
        # names no download.
        application('php-lib/greeting', {'composer.json': json.dumps({'name': 'example/greeting', 'version': '1.0.0'})})
        application('php-lib/devtool', {'composer.json': json.dumps({'name': 'example/devtool', 'version': '1.0.0'})})
        php_app = application('php-app', {'composer.json': json.dumps({
            'name': 'example/app', 'repositories': [{'type': 'path', 'url': '../php-lib/*', 'options': {'symlink': False}}, {'packagist.org': False}],
            'require': {'example/greeting': '1.0.0'}, 'require-dev': {'example/devtool': '1.0.0'}})})
        as_app(php_app, 'composer update --no-interaction --no-progress --quiet && rm -rf vendor')

        def vendor_installed():
            remote(f'test -f {php_app}/vendor/example/greeting/composer.json\ntest ! -e {php_app}/vendor/example/devtool\n'
                   f'test -f {php_app}/vendor/autoload.php')

        project([{'id': 'dependencies', 'composer_project_dependencies': {'lock_file_dir': php_app, 'run_as_user_account': ACCOUNT, 'include_development_dependencies': False}}])
        operations = task(run('dryrun'), 'dependencies')['value']['operations']
        assert any('example/greeting' in operation for operation in operations), operations
        lock_file_module('composer_project_dependencies', php_app, vendor_installed, include_development_dependencies=False)
        mark('composer_project_dependencies installs what composer.lock lists without require-dev, lists the operations in dryrun, and reruns unchanged')

        # uv: a project without a build system, so uv installs only its
        # dependencies, with the Python that the node has.
        uv_app = application('uv-app', {'pyproject.toml': PYPROJECT})
        as_app(uv_app, f'{UV} lock --quiet')
        venv_python = uv_app + '/.venv/bin/python'

        def uv_installed():
            as_app(uv_app, f'{venv_python} -c "import six"')
            remote(f'test -z "$(ls -d {uv_app}/.venv/lib/python*/site-packages/iniconfig 2>/dev/null)"')
            assert remote(f'find {uv_app}/.venv ! -user {ACCOUNT} | wc -l') == '0'

        _, record = lock_file_module('uv_project_dependencies', uv_app, uv_installed, include_development_dependencies=False, uv_program_path=UV)
        assert record['lock_file'] == 'uv.lock' and record['versions'][0].startswith('uv uv 0.13.0'), record
        # A Python that uv installs, as the recipe shows, named in
        # python_program_path: a new setting, so the packages install again.
        machine = {'x86_64': 'x86_64', 'aarch64': 'aarch64'}[remote('uname -m')]
        python = f'{UV_PYTHON_DIRECTORY}/cpython-{UV_PYTHON_VERSION}-linux-{machine}-gnu/bin/python3'
        with_python = {'id': 'dependencies', 'uv_project_dependencies': {'lock_file_dir': uv_app, 'run_as_user_account': ACCOUNT,
                       'include_development_dependencies': False, 'uv_program_path': UV, 'python_program_path': python}}
        project([{'shell': {'script': f'{UV} python install --no-bin {UV_PYTHON_VERSION}', 'env': {'UV_PYTHON_INSTALL_DIR': UV_PYTHON_DIRECTORY}, 'skip_if_this_path_exists': python}},
                 with_python])
        installed = task(run('run'), 'dependencies')
        assert installed['change'] == 'changed' and installed['reason'] == "installed: the task's settings changed since the last installation", installed
        assert as_app(uv_app, f'{venv_python} --version') == 'Python ' + UV_PYTHON_VERSION
        uv_installed()
        unchanged_rerun('dependencies')
        # Development is explicit even when uv's configured default groups omit it.
        development_manifest = PYPROJECT + '\n[tool.uv]\ndefault-groups = []\n'
        remote(f'printf %s {shlex.quote(development_manifest)} > {uv_app}/pyproject.toml')
        as_app(uv_app, f'{UV} lock --quiet')
        with_python['uv_project_dependencies']['include_development_dependencies'] = True
        project([with_python])
        assert task(run('run'), 'dependencies')['change'] == 'changed'
        as_app(uv_app, f'{venv_python} -c "import iniconfig"')
        unchanged_rerun('dependencies')
        with_python['uv_project_dependencies']['include_development_dependencies'] = False
        project([with_python])
        assert task(run('run'), 'dependencies')['change'] == 'changed'
        uv_installed()
        # A manifest-only change in this recorded directory must reach uv's
        # stale-lock check and keep the successful installation record.
        record_path = uv_app + '/.mmx-uv-project-dependencies'
        previous_record = remote(f'cat {record_path}')
        stale_manifest = development_manifest.replace('six==1.16.0', 'six==1.17.0')
        remote(f'printf %s {shlex.quote(stale_manifest)} > {uv_app}/pyproject.toml')
        project([with_python])
        stale = task(run('run', 1), 'dependencies')
        assert stale['status'] == 'failed' and 'needs to be updated' in stale['reason'], stale
        assert remote(f'cat {record_path}') == previous_record
        mark(f'uv_project_dependencies explicitly includes or excludes the dev group, reruns unchanged, installs again with a Python '
             f'that uv installed (Python {UV_PYTHON_VERSION}), and fails on a lock file that pyproject.toml no longer fits')

        # A missing lock file is a configuration error that names it.
        bare = application('bare', {'package.json': json.dumps(PACKAGE_JSON)})
        project([{'id': 'dependencies', 'npm_project_dependencies': {'lock_file_dir': bare, 'run_as_user_account': ACCOUNT, 'include_development_dependencies': False, 'npm_program_path': NPM}}])
        refused = task(run('run', 1), 'dependencies')
        assert refused['status'] == 'failed' and 'has no package-lock.json or npm-shrinkwrap.json' in refused['reason'], refused
        mark('a lock file module without its lock file fails and names it')

        # The example project: deploy a Python application from Git, one
        # directory for each release.
        example = vm.ROOT / 'deploy-from-git-example'
        if example.exists():
            shutil.rmtree(example)
        shutil.copytree(vm.REPO / 'examples/deploy-from-git-with-release-directories', example)
        source = example / 'example-application'
        repository = BASE + '/greeter.git'
        remote(f'mkdir -p {repository}\ncd {repository}\ngit init -q -b main\n'
               'git config user.name MMX\ngit config user.email mmx@example.com\n'
               + ''.join(f"cat > {name} <<'EOF'\n{(source / name).read_text()}EOF\n" for name in ('app.py', 'requirements.txt'))
               + "printf 'v1\\n' > RELEASE\ngit add .\ngit commit -q -m v1\ngit tag v1\n"
               "printf 'v2\\n' > RELEASE\ngit commit -q -am v2\ngit tag v2\n"
               # Git fetches from a repository of another account only when
               # told it is safe; the test's repository belongs to root.
               f'git config --system --add safe.directory {repository}\n'
               f'git config --system --add safe.directory {repository}/.git')
        vm.write_inventory(example, {'nodes': {'app': node}, 'vars': {'greeter_repository': repository}}, name='example')

        def deploy(command, release):
            result = vm.cmd([mmx, command, 'deploy', '--var', 'release=' + release, '--json'], cwd=example, check=False)
            assert result.returncode in vm.success_codes(command), (command, result.stdout, result.stderr)
            return json.loads(result.stdout)['entries'][0]['response']

        def greeter_pid():
            return remote('systemctl show -p MainPID --value greeter')

        try:
            assert deploy('dryrun', 'v1')['change'] == 'would_change'
            deploy('run', 'v1')
            assert remote('curl -s http://127.0.0.1:8080/') == 'Hello from release v1'
            first = greeter_pid()
            assert deploy('run', 'v1')['change'] == 'unchanged' and greeter_pid() == first
            deploy('run', 'v2')
            assert remote('curl -s http://127.0.0.1:8080/') == 'Hello from release v2' and greeter_pid() != first
            assert remote('readlink /srv/greeter/current') == 'releases/v2' and remote('ls /srv/greeter/releases') == 'v2'
            assert deploy('run', 'v2')['change'] == 'unchanged'
            mark('the example deploy-from-git-with-release-directories deploys v1, reruns unchanged, and switches to v2 with one restart')
        finally:
            remote('systemctl disable --now greeter 2>/dev/null || true\nrm -f /etc/systemd/system/greeter.service\nsystemctl daemon-reload\n'
                   'rm -rf /srv/greeter\nuserdel greeter 2>/dev/null || true\ngroupdel greeter 2>/dev/null || true\n'
                   f'git config --system --unset-all safe.directory "^{repository}" || true')
    finally:
        cleanup()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} language installer workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
