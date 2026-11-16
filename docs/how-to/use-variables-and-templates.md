# Use variables and templates

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. No command on this
page asks a question, so a coding agent can run every command as written.

**When to use this page:** a playbook needs values that differ between
servers or between runs, such as a port, a release version, or a cache size
that depends on the server's memory. Or a configuration file is long, and
you want to keep the file's text out of the playbook.

**What this page does:** you grow the playbook `set_up_web_servers` from
[Write a playbook](write-a-playbook.md). The nginx site reads its port from
a variable. The home page comes from a template file. A
[memcached](https://memcached.org/) cache is
sized from each server's memory and CPUs. A release version comes from the
command line. Then `mmx explain` shows where each value came from.

**Terms:**

- A **node** is a Linux server that MMX manages. A **playbook** lists
  **fleet stages**. Each fleet stage picks nodes, and each of those nodes
  runs the fleet stage's **node tasks**, in order.
- A **variable** is a named value, such as `http_port: 8080`. A node task
  reads the variable as `{{ .vars.http_port }}`. Text in `{{ }}` that names
  a value is a **reference**.
- A **template** is a file that MMX fills in for each node, such as
  `files/index.html.tmpl`. A template uses Go's
  [`text/template`](https://pkg.go.dev/text/template) syntax.
- A **fact** is a value that the node reports about itself, such as its
  number of CPUs. A node task reads a fact as `{{ .node.cpu.count }}`.
- A **command-line variable** is a variable that the playbook declares and
  you set for each run with `--var`, such as `--var release=1.4.3`.

| What you need | Where |
| --- | --- |
| A value that is the same on many servers, such as a port | [Step 1](#steps), and [Describe your servers](describe-your-servers.md) for where to set the value |
| A whole configuration file built from values | [Step 2](#steps) |
| A setting sized from each server's memory or CPUs | [Step 3](#steps) |
| A value that changes with each run, such as a release version | [Step 4](#steps) |
| Every value that a server gets, and where each value came from | [Step 5](#steps) |
| A value computed during the run | [Values computed during the run](#values-computed-during-the-run) |
| A number, a list, or a mapping for a module input | [A number, a boolean, a list, or a mapping from a reference](#a-number-a-boolean-a-list-or-a-mapping-from-a-reference) |
| Text with `{{` for another tool, such as Docker | [A literal `{{` for another tool](#a-literal--for-another-tool) |
| Values about the other servers, such as their addresses | [Values about other servers](#values-about-other-servers) |

## Before you start

- `playbooks/set_up_web_servers.yml` is the playbook from
  [Write a playbook](write-a-playbook.md), and `mmx run set_up_web_servers`
  succeeds. The servers `web01` and `web02` are in the group `web_servers`
  in `inventories/example.yml`.
- The project sets the variables from
  [Describe your servers](describe-your-servers.md), step 3. This page uses
  two of the variables: `site_name: shop` in the `vars` of
  `mmx_project_settings.yml`, and `http_port: 8080` in the `vars` of the
  group `web_servers`.
- The playbook has the fleet stage `set_up_cache` from
  [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md),
  step 8. Step 3 changes that fleet stage.
- The servers run Debian or Ubuntu, because the examples install packages
  with `apt_package`.

The examples use `web01` at `203.0.113.11` with 2 CPUs and 2048 MiB of
memory, and `web02` at `203.0.113.12` with 4 CPUs and 4096 MiB of memory.
The line numbers in the output of `mmx explain` depend on your files, so
your numbers may differ from the numbers on this page.

## Steps

1. **Read the port from a variable.** In
   `playbooks/set_up_web_servers.yml`, replace the two places that say
   `8080`: the `listen` line of `write_site_file`, and the URL of
   `check_home_page`.

   ```yaml
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   content: |
                     server {
                       listen {{ .vars.http_port }};
                       root /srv/shop/public;
                     }
   ```

   ```yaml
               - id: check_home_page
                 wait_for:
                   url_responds: "http://127.0.0.1:{{ .vars.http_port }}/"
                   response_body_contains: Shop
   ```

   Put quotes around a YAML value that starts with `{{`, such as
   `uid: "{{ .vars.shop_user_id }}"` in
   [A number, a boolean, a list, or a mapping from a reference](#a-number-a-boolean-a-list-or-a-mapping-from-a-reference).
   Without the quotes, YAML reads `{` as the start of a mapping. A value
   that starts with other text, such as the URL here, needs no quotes.

   **Check:** `mmx explain set_up_web_servers --limit web01` shows the
   value that each field gets, and the inventory line that supplied the
   variable. `mmx explain` contacts no server.

   ```text
     web01 / set_up_nginx/shop_site/changes/write_site_file: file (ready)
       at playbooks/set_up_web_servers.yml:25:15 (fleet_stages[0].node_tasks[3].task_block.changes[0])
       file.content = "server {\n  listen 8080;\n  root /srv/shop/public;\n}\n"
         from playbooks/set_up_web_servers.yml:29:26 (fleet_stages[0].node_tasks[3].task_block.changes[0].file.content)
         value from inventories/example.yml:18:18 (groups.web_servers.vars.http_port)
   ```

   Then `mmx dryrun set_up_web_servers` reports every node task as
   `unchanged`, with exit code 0, because the site file has the same bytes
   as before. If `mmx explain` says
   `reference .vars.http_port is not declared for node web01` (the message then
   lists where to declare it),
   the node does not get the variable: see
   [If something is different](#if-something-is-different).

2. **Write the home page from a template file.** Create
   `files/index.html.tmpl` in the project:

   ```text
   <h1>{{ .vars.site_name | title }}</h1>
   <p>Served by {{ .node.name }}.</p>
   ```

   `.node.name` is the node's name in the inventory, such as `web01`.
   `| title` sends the value through the function `title`, which makes the
   first letter of each word a capital: `shop` becomes `Shop`.
   [Functions in templates](#functions-in-templates) lists the functions.

   Then replace the `content` of `write_home_page` with `template`:

   ```yaml
         - id: write_home_page
           file:
             path: /srv/shop/public/index.html
             permissions_mode: "0644"
             template: files/index.html.tmpl
   ```

   A `template` path in a playbook starts at the project directory, the
   directory that holds `mmx_project_settings.yml`. MMX reads the template
   file on your computer and sends the file with the run. Each node fills in
   the template with the node's own values.

   **Check:** `mmx explain set_up_web_servers --limit web01` shows the
   page that `web01` gets:

   ```text
       file.template, rendered for web01:
         <h1>Shop</h1>
         <p>Served by web01.</p>
   ```

   Then `mmx run set_up_web_servers --diff` updates the page on each
   server, each with the server's own name:

   ```text
     set_up_nginx / web01 / write_home_page: succeeded, changed
     set_up_nginx / web01 / write_home_page: updated "/srv/shop/public/index.html"
       14 bytes, permissions_mode 0644, uid 0, gid 0 -> 38 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,1 +1,2 @@
        <h1>Shop</h1>
       +<p>Served by web01.</p>
   ```

   The run ends with `Run ...: succeeded` and exit code 0. `check_home_page`
   still succeeds, because the page still contains `Shop`.

3. **Size a cache from each server's memory and CPUs.** The playbook
   already has the fleet stage `set_up_cache` from
   [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md),
   step 8. The fleet stage writes a fixed memcached configuration. Here
   the cache size becomes a share of each server's memory, so a larger
   server gets a larger cache. First add the share to the group's
   variables in `inventories/example.yml`:

   ```yaml
   # inventories/example.yml, in groups
     web_servers:
       nodes: [web01, web02]
       vars:
         http_port: 8080
         worker_count: 4
         cache_percent: 25          # a variable name you choose
   ```

   Create `files/memcached.conf.tmpl`:

   ```text
   # memcached settings, sized for this server by MMX
   -m {{ percent .node.memory.total_mib .vars.cache_percent }}
   -t {{ .node.cpu.count }}
   -p 11211
   -u memcache
   -l 127.0.0.1
   ```

   `-m` is memcached's cache size in megabytes, and `-t` is its number of
   threads. `.node.memory.total_mib` is the memory that the kernel can use,
   in MiB: a little less than the installed memory. `.node.cpu.count` is
   the server's number of CPUs. `percent` returns the given percentage of
   a number, rounded down to a whole number. Then, in the fleet stage
   `set_up_cache`, change `write_memcached_config` from `content` to
   `template`:

   ```yaml
               - id: write_memcached_config
                 file:
                   path: /etc/memcached.conf
                   permissions_mode: "0644"
                   template: files/memcached.conf.tmpl
   ```

   **Check:** preview the fleet stage. `--only-fleet-stages` runs only
   the named fleet stage. The fleet stage restarts memcached, so the
   dryrun needs the approval from step 9 of
   [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
   without `--var restart_approved=true`, `require_restart_approval` fails
   on both servers and the dryrun exits with code 1.

   ```sh
   mmx dryrun set_up_web_servers --diff --only-fleet-stages set_up_cache --var restart_approved=true
   echo "exit code $?"
   ```

   Each server gets its own numbers: `web01` has 2048 MiB and 2 CPUs, and
   `web02` has 4096 MiB and 4 CPUs. The exit code is 2, "something would
   change". The memcached configuration from step 9 says `-m 128`, and
   the template replaces it:

   ```text
     set_up_cache / web01 / shop_cache/changes/write_memcached_config: would update "/etc/memcached.conf"
       41 bytes, permissions_mode 0644, uid 0, gid 0 -> 97 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,4 +1,6 @@
       --m 128
       +# memcached settings, sized for this server by MMX
       +-m 512
       +-t 2
        -p 11211
        -u memcache
        -l 127.0.0.1
     set_up_cache / web01 / shop_cache/activate/restart_memcached: succeeded, would_change — tracked input /etc/memcached.conf changed after the service last started or reloaded
     ...
     set_up_cache / web02 / shop_cache/changes/write_memcached_config: would update "/etc/memcached.conf"
       41 bytes, permissions_mode 0644, uid 0, gid 0 -> 98 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,4 +1,6 @@
       --m 128
       +# memcached settings, sized for this server by MMX
       +-m 1024
       +-t 4
        -p 11211
        -u memcache
        -l 127.0.0.1
   ```

   Then run the fleet stage:

   ```sh
   mmx run set_up_web_servers --only-fleet-stages set_up_cache --var restart_approved=true
   ```

   Each server writes its file and restarts memcached, with the reason
   `tracked input /etc/memcached.conf changed after the service last started or reloaded`.
   On `web01`, `ps -o args= -C memcached` then prints
   `/usr/bin/memcached -m 512 -t 2 -p 11211 -u memcache -l 127.0.0.1`.
   A second run, without `--var`, says `unchanged` for every node task
   except `require_restart_approval`, which says `skipped` because nothing
   needs a restart.

   After you give a server more memory, the next run writes the new size
   and restarts memcached, so the run needs `--var restart_approved=true`
   again. Here `web01` went from 2048 MiB to 4096 MiB:

   ```text
     set_up_cache / web01 / shop_cache/changes/write_memcached_config: updated "/etc/memcached.conf"
       97 bytes, permissions_mode 0644, uid 0, gid 0 -> 98 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,5 +1,5 @@
        # memcached settings, sized for this server by MMX
       --m 512
       +-m 1024
        -t 2
        -p 11211
        -u memcache
     set_up_cache / web01 / shop_cache/activate/restart_memcached: succeeded, changed — tracked input /etc/memcached.conf changed after the service last started or reloaded
   ```

4. **Take the release version from the command line.** A value that
   changes with each run, such as the release that a deployment installs,
   belongs on the command line. Declare the value at the top of the
   playbook, above `fleet_stages`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   command_line_vars:
     release:                  # a command-line variable name you choose
       type: string
   ```

   Add the release to the home page, in `files/index.html.tmpl`:

   ```text
   <h1>{{ .vars.site_name | title }}</h1>
   <p>Served by {{ .node.name }}. Release {{ .vars.release }}.</p>
   ```

   Node tasks and templates read a command-line variable as `.vars.release`,
   like any other variable. The value is the same on every node. Then run
   the playbook with the value:

   ```sh
   mmx run set_up_web_servers --var release=1.4.3
   ```

   **Check:** the run says `updated "/srv/shop/public/index.html"` on each
   server, and ends with `Run ...: succeeded`. On `web01`,
   `curl http://127.0.0.1:8080/` prints:

   ```text
   <h1>Shop</h1>
   <p>Served by web01. Release 1.4.3.</p>
   ```

   A run with the same `--var` value changes nothing. Without `--var`,
   `mmx dryrun`, `mmx run`, and `mmx explain` stop before they contact a
   server, with exit code 4:

   ```text
   mmx: set release with --var NAME=VALUE; command_line_vars gives no default
   ```

   To make the value optional, give it a `default`, such as
   `default: "1.4.2"`. If the playbook already has `command_line_vars`,
   such as `restart_approved` from
   [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md),
   add `release` below the variables that the playbook declares.

5. **See every value that a server gets, and where each value came
   from.**

   ```sh
   mmx explain set_up_web_servers --var release=1.4.3 --limit web01
   ```

   **Check:** the output starts with the command-line variables, then lists
   each node task with each value and its source. A template that reads
   only variables and the node's name is shown filled in. A template that
   reads a fact is not, because only the node knows its facts:

   ```text
   Project mmx_example
   Limit web01
   Command-line variable release = "1.4.3"
     from --var release

   set_up_nginx → web01
     ...
       file.template, rendered for web01:
         <h1>Shop</h1>
         <p>Served by web01. Release 1.4.3.</p>
     ...
       file.template, not rendered by mmx explain: the template reads .node.memory.total_mib, a fact that the node reports when MMX connects to it
   ```

   A `value from` line names the file and line that set a variable, such
   as `inventories/example.yml:18:18 (groups.web_servers.vars.http_port)`.
   If a value is not the one you expect, change the value in the file
   and line that the `value from` line names.
   `mmx explain --json` prints the same in JSON, for a script or a coding
   agent: the command-line variables are in `command_line_vars`, each with
   its `value` and `source`.

## The whole files

`playbooks/set_up_web_servers.yml` after the five steps. The fleet stage
`set_up_cache` and the command-line variable `restart_approved` come from
[Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):

```yaml
# playbooks/set_up_web_servers.yml
command_line_vars:
  restart_approved:
    type: boolean
    default: false
  release:                  # a command-line variable name you choose
    type: string

fleet_stages:
  - id: set_up_nginx
    nodes: web_servers
    node_tasks:
      - id: install_nginx
        apt_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}

      - id: create_site_directory
        directory:
          path: /srv/shop/public
          create_missing_parent_directories: true

      - id: write_home_page
        file:
          path: /srv/shop/public/index.html
          permissions_mode: "0644"
          template: files/index.html.tmpl

      - task_block:
          id: shop_site
          changes:
            - id: write_site_file
              file:
                path: /etc/nginx/conf.d/shop.conf
                permissions_mode: "0644"
                content: |
                  server {
                    listen {{ .vars.http_port }};
                    root /srv/shop/public;
                  }
          activate:
            - id: reload_nginx
              systemd_service:
                name: nginx
                start_at_boot: true
                state: running
                refresh: reload
          verify:
            - id: check_home_page
              wait_for:
                url_responds: "http://127.0.0.1:{{ .vars.http_port }}/"
                response_body_contains: Shop

  # The fleet stage set_up_cache stays as in "Change a configuration and
  # reload its service", with `template: files/memcached.conf.tmpl` in place
  # of `content` in write_memcached_config.
```

`files/index.html.tmpl`:

```text
<h1>{{ .vars.site_name | title }}</h1>
<p>Served by {{ .node.name }}. Release {{ .vars.release }}.</p>
```

`files/memcached.conf.tmpl`:

```text
# memcached settings, sized for this server by MMX
-m {{ percent .node.memory.total_mib .vars.cache_percent }}
-t {{ .node.cpu.count }}
-p 11211
-u memcache
-l 127.0.0.1
```

## Where a value comes from

A node task reads values through references. Each kind of value has its
own start:

| Reference | Where the value comes from |
| --- | --- |
| `.vars.NAME` | The `vars` of the project, the inventory, the node's groups, and the node. [Describe your servers](describe-your-servers.md#steps), step 3, shows the four places and the order in which a later place replaces an earlier value. |
| `.vars.NAME` for a command-line variable | `--var NAME=VALUE`, or else the variable's `default` in the playbook. See [Command-line variables](#command-line-variables). |
| `.vars.NAME` from `set_vars` | A node task earlier in the same fleet stage. See [Values computed during the run](#values-computed-during-the-run). |
| `.node.NAME` | A fact that the node reports. See [Node facts](#node-facts). |
| `.inventory.nodes.NAME` and `.inventory.groups.NAME` | The inventory. See [Values about other servers](#values-about-other-servers). |
| `.inputs.NAME` | A role's input, inside a role. See [Reuse work with roles](reuse-work-with-roles.md). |
| `.node_tasks.ID` | The result of an earlier node task of the same fleet stage on the same node, such as `.node_tasks.write_home_page.change`. The [Use a result from one server on another](use-a-result-from-one-server-on-another.md) lists the results that a node task can read. |

These rules hold for every reference:

- A missing value is an error, never empty text. A reference to a variable
  that the node does not get fails before MMX contacts a server, and the
  error names the file, line, and field.
- Names are case-sensitive: `.vars.HTTP_port` is not `.vars.http_port`.
- A reference inside other text, such as `listen {{ .vars.http_port }};`,
  makes text. A value that is exactly one reference keeps the value's type,
  such as a number or a list.
- MMX fills in a value once. A variable whose value is
  `"{{ .vars.site_name }}"` gives the text `{{ .vars.site_name }}`, not
  the site name.
- A `shell` node task's `script` is not filled in: MMX refuses a reference
  in `script`, and the message says how to pass the value through `env`.

## Command-line variables

- `type` is `string`, `integer`, or `boolean`. A string is taken as
  written, an integer is a decimal number, and a boolean is `true` or
  `false`. `--var release=1.4.3` sets the text `1.4.3`: MMX does not read
  the value as YAML or as a reference.
- `default` is optional. A variable without a `default` must be set with
  `--var` on `mmx explain`, `mmx dryrun`, and `mmx run`.
- Repeat `--var` for each variable: `--var release=1.4.3 --var run_migrations=false`.
  The text after the first `=` is the value.
- `mmx validate set_up_web_servers` and `mmx nodes set_up_web_servers`
  accept `--var` and need no value. Without a playbook name, the two
  commands refuse `--var` with `mmx: --var requires a playbook name`.
  `mmx validate` without a playbook name still checks a playbook with a
  command-line variable that has no default, as far as MMX can without the
  value.
- A field that MMX reads on your computer before the run starts, such as
  the URL of a `download` node task, needs the value to be checked. Check
  such a field with `mmx validate set_up_web_servers --var release=1.4.3`.
- `mmx resume` finishes a run's unfinished work with the values that the
  run used, and refuses `--var`. A new `mmx run` uses only the values that you
  give the new command, and the current defaults.
- A command-line variable's name must not also be in the `vars` of the
  project, the inventory, a group, or a node, so the value at the start of
  a run has one source.
- Command-line values are not secret. Command-line values show in the
  list of running processes, in `mmx explain`, and in the run's saved
  history in `.mmx/`.
  Pass a password as a secret instead, as
  [Keep secrets](keep-secrets.md) explains.

In CI, pass the value from the CI system, such as
`mmx run set_up_web_servers --var release="$RELEASE_VERSION"`. MMX refuses an
empty value for a command-line variable that has no `default`: when
`RELEASE_VERSION` is not set, the command stops with `--var release= gives an
empty value` before any server is contacted, and not with a home page that
says `Release .`. A variable that may be empty declares `default: ""` in
`command_line_vars`.

## Node facts

MMX collects each node's facts when the node starts work on a fleet stage,
before any node task, in a dryrun too. A resume and a new run collect the
facts again. The facts that you may use most:

| Fact | Value on `web01` |
| --- | --- |
| `.node.name` | The node's name in the inventory: `web01`. |
| `.node.cpu.count` | The number of CPUs that are online: `2`. On a virtual machine, its virtual CPUs. |
| `.node.memory.total_mib` | The memory that the kernel can use, from `MemTotal` in `/proc/meminfo`, in MiB, rounded down: `2048`. It is a little less than the installed memory. Swap does not count, and the value is not the free memory. `.node.memory.total_bytes` is the same in bytes. |
| `.node.distribution.id` | The distribution, from `/etc/os-release`: `debian`, `ubuntu`, `rocky`, `almalinux`, `arch`, or `opensuse-leap`, for example. |
| `.node.distribution.version` | The release as text: `13`, or `26.04` on Ubuntu 26.04. |
| `.node.distribution.major_version` | The release number before the first dot, as a number: `13`. |
| `.node.distribution.codename` | The release's code name, where the distribution has one: `trixie`. |
| `.node.distribution.family` | `debian`, `redhat`, `arch`, or `suse`, such as `debian` for Debian and Ubuntu and `redhat` for Rocky Linux, AlmaLinux, Fedora, and RHEL. A distribution that MMX does not list takes the family that the `ID_LIKE` line of its `/etc/os-release` names. |
| `.node.package_manager` | `apt`, `dnf`, `pacman`, or `zypper`: `apt`. |
| `.node.arch` | The processor architecture: `amd64` or `arm64`. |
| `.node.fully_qualified_name` | The server's full host name with its domain, such as `web01.example.net`. |
| `.node.network.default_ipv4_address` | The address that the server sends its default route's packets from. |

The [node facts reference](../reference/playbook-file.md#node-facts)
lists every fact, such as the kernel, the network interfaces, and the
mounted filesystems, and what a missing fact does. To see a node's facts, show the facts in a node task,
such as:

```yaml
- show: "{{ .node.distribution.id }} {{ .node.distribution.version }} ({{ .node.distribution.codename }}), {{ .node.cpu.count }} CPUs, {{ .node.memory.total_mib }} MiB memory"
```

A run then prints `"debian 13 (trixie), 2 CPUs, 2048 MiB memory"` for
`web01`. `mmx results --json` also shows every fact that each node
reported, in `facts.values` of each entry's `response`.

Size settings from total capacity, as the reference says, not from free
memory: free memory changes with load, so a setting sized from it would
change on every run.

### Find a storage device

`.node.storage.block_devices` lists the node's disks, partitions, LVM
logical volumes, and other block devices, with each device's size, even
when a device is not mounted. Each entry has `name`, `path` (such as
`/dev/sdb`), `kind`, `size_bytes`, and `read_only`. A kernel path such as
`/dev/sdb` can name a different disk after a reboot. So find the disk by a
path in its `stable_paths_by_id`, from `/dev/disk/by-id/`, and check
`size_bytes` against the size that you expect.

[Block-device fields](../reference/playbook-file.md#block-device-fields)
lists the other fields of an entry, and `.node.storage.mounts` lists the
mounted filesystems.

## Functions in templates

A template file can call the functions of
[Sprig](https://masterminds.github.io/sprig/), the function library that
[Helm](https://helm.sh/) templates use, with Sprig's names and argument order. MMX adds two
functions of its own:

- `percent` returns a percentage of a number, rounded down to a whole
  number. The percentage must be a whole number from 0 to 100:
  `{{ percent .node.memory.total_mib 25 }}` is `512` for 2048 MiB.
- `toYaml` writes a value as YAML with sorted keys, as in Helm.

```text
workers = {{ .node.cpu.count }}
cache_mib = {{ percent .node.memory.total_mib .vars.cache_percent }}
replica_parallel_workers = {{ div .node.cpu.count 2 }}
Ciphers {{ join "," .vars.ssh_ciphers }}

scrape_configs:
{{ .vars.scrape_configs | toYaml | indent 2 }}
```

Here `ssh_ciphers` is a list variable and `scrape_configs` a list of
mappings. Sprig functions that you may use often: `default`, `join`,
`split`, `upper`, `lower`, `title`, `quote`, `indent`, `add`, `sub`,
`mul`, `div`, `max`, `min`, `b64enc`, `sha256sum`, `toJson`, `dict`,
`list`, `hasKey`, and `ternary`.

After you resize a server, the next run renders the template from the new capacity. MMX compares the file and the record of what each service last loaded, and changes only what differs.

MMX leaves out the Sprig functions that depend on more than their
arguments, such as `now`, `randAlphaNum`, and `env`, so that a run with
nothing new reports `unchanged`. [Template functions](../reference/playbook-file.md#template-functions)
lists them. For a random password, make the password once and keep the
password as a secret, as [Keep secrets](keep-secrets.md) explains.

Functions work in template files only. A value in the playbook's YAML
takes references, not function calls.

## Values computed during the run

`set_vars` gives a variable a value at a point in the node task list. Later
node tasks of the same fleet stage read the variable as `.vars.NAME`:

```yaml
- set_vars:
    large_server:
      number_at_least: ["{{ .node.memory.total_mib }}", 4096]
- when: "{{ .vars.large_server }}"
  show: "{{ .node.name }} has at least 4096 MiB of memory"
```

A value that is one condition, such as `number_at_least` or `equals`,
becomes `true` or `false`. In this example, the `show` node task says
`skipped, unchanged — when is false` on `web01`, and prints
`"web02 has at least 4096 MiB of memory"` on `web02`. The conditions are
listed in [Conditions](../reference/shared-module-behavior.md#conditions).

- A `set_vars` value lasts until the end of the node's fleet stage, loop
  item, or role. A later fleet stage gets values through outputs, which
  [Use a result from one server on another](use-a-result-from-one-server-on-another.md) explains.
- The [`set_vars` reference](../reference/playbook-file.md#set_vars) lists
  the other rules, such as what `set_vars` can replace.

## A number, a boolean, a list, or a mapping from a reference

A module input that takes a number, a boolean, a list, or a mapping also
takes text that is exactly one reference. The reference keeps the type of
the value that it reads. For example, with `shop_user_id: 1500` in the
project's `vars`:

```yaml
- id: create_shop_account
  user:
    name: shop
    uid: "{{ .vars.shop_user_id }}"
    create_home_directory: false
```

`mmx explain` shows `user.uid = 1500` and the line of
`mmx_project_settings.yml` that set the value, and the run creates the
account `shop` with user ID 1500.

- MMX checks a variable or a role's input on your computer, in
  `mmx validate`, `mmx explain`, `mmx dryrun`, and `mmx run`. A value that
  only the node knows, such as a fact, is checked on the node before the
  module runs.
- The value must have the input's type and pass the module's own rules,
  such as an account ID in range. The
  [reference](../reference/playbook-file.md#a-number-a-boolean-a-list-or-a-mapping-from-a-reference)
  lists the errors and the inputs that take no whole-value reference.

## A literal `{{` for another tool

Some tools use `{{` in their own syntax, such as Docker's `--format`. To
write `{{` into a value that takes references, put the text in back quotes
inside braces:

```yaml
- id: write_container_list_script
  file:
    path: /usr/local/bin/show-containers
    permissions_mode: "0755"
    content: |
      #!/bin/sh
      docker ps --format '{{`{{.Names}}`}}: {{`{{.Status}}`}}'
```

The file on the node then says
`docker ps --format '{{.Names}}: {{.Status}}'`. A template file uses the
same form. A `shell` script and a `source` file are copied as written, so
they need no back quotes; the
[reference](../reference/playbook-file.md#double-braces-in-scripts-and-in-other-text)
lists them.

## Values about other servers

`.inventory.nodes.NAME` holds each node's `name`, `address`, and `vars`.
`.inventory.nodes.NAME.vars` holds the variables of the node's groups and
of the node itself. The project's and the inventory's variables are the
same for every node, so read the project's and the inventory's variables
through `.vars`. `.inventory.groups.NAME` is the sorted list
of a group's node names. For example, this template lists the web servers
with their addresses and ports:

```text
{{ range .inventory.groups.web_servers -}}
{{ . }} {{ (index $.inventory.nodes .).address }} port {{ (index $.inventory.nodes .).vars.http_port }}
{{ end -}}
```

`mmx explain` shows the file that each node gets:

```text
    file.template, rendered for web01:
      web01 203.0.113.11 port 8080
      web02 203.0.113.12 port 8080
```

The inventory holds only what you wrote. A value that a server reports
during the run, such as the version of an installed program, reaches
another server through a fleet stage's outputs, which
[Use a result from one server on another](use-a-result-from-one-server-on-another.md) explains.

## A file for each server

The `source` and `template` paths of a `file` node task may contain
references, so one node task can send a different file to each server:

```yaml
- file:
    path: /etc/haproxy/haproxy.cfg
    permissions_mode: "0644"
    template: "files/haproxy/{{ .node.name }}.cfg.tmpl"
```

Here `web01` gets `files/haproxy/web01.cfg.tmpl`. A path may read only
what your computer knows before MMX contacts a server: `.node.name`,
variables, a role's inputs, a loop's items, and `.inventory`. A path that
reads a fact fails:

```text
template reads .node.distribution.id, which is known only during a run; this field may read the node's name (.node.name), vars, role inputs, and loop items, which the controller machine knows before it connects
```

For a file that differs by a fact, such as `.node.arch`, write one node
task for each value, each with a `when`. Before MMX contacts a server,
MMX checks that each path names a file that exists.
[The `file` module page](../reference/modules/file.md#paths-on-the-controller-machine)
says more.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `reference .vars.http_port is not declared for node web01`, or `... is declared for node web01 but not for node db01` | The node does not get the variable, or the name is misspelled. The message lists where to declare the variable, and names the nodes that already get it. In a fleet stage's `when`, `set_vars` cannot declare the variable, because MMX reads the `when` before the fleet stage's node tasks run; in `nodes`, `except_nodes`, `loop`, and `for_each_node`, which MMX reads before it chooses a node, only `vars` in `mmx_project_settings.yml`, `vars` at the top level of the inventory, and `command_line_vars` count. Check the name, and that the node's groups set the variable. `mmx explain --limit NODE` shows which values the node gets. |
| `mmx: set release with --var NAME=VALUE; command_line_vars gives no default` | The playbook declares `release` without a `default`. Add `--var release=VALUE`, or give the variable a `default`. |
| `mmx: --var relese=2: relese is not declared in command_line_vars (declared: release)` | A misspelled name, or a variable that the playbook does not declare. The message lists the declared names. |
| `mmx: --var run_checks=yes: run_checks must be true or false (declared: release, run_checks)` | A boolean takes only `true` or `false`. |
| `mmx: --var release=1: release is set more than once (declared: release)`, or `... must be a decimal integer` | `--var` was repeated for one name, or an integer variable got text that is not a decimal number. These errors, like an undeclared name, stop the command before MMX connects to any server. |
| `command-line variable release must not also be set in vars` | The name is in `command_line_vars` and in some `vars`. Remove the name from the `vars` that the message names. |
| `template function now is not available: the function reads the clock, random values, the environment, or the network` | A template calls a function that MMX leaves out. See [Functions in templates](#functions-in-templates). |
| `reference must start with a dot: "add .vars.http_port 1"` | A function call in the playbook's YAML. Move the text to a template file, or write the computed value as a variable. |
| A node task fails with `error calling percent: percent requires a percentage between 0 and 100` | The percentage, such as `cache_percent: 250`, is outside 0 to 100. Because the template reads a fact, the error shows on the node during the dryrun or run, not in `mmx validate`. |
| `mmx explain` says `not rendered by mmx explain: the template reads .node.memory.total_mib, a fact that the node reports when MMX connects to it` | Nothing is wrong. `mmx dryrun --diff` shows what each server gets. |

## Where to go next

- [Describe your servers](describe-your-servers.md): where to set project,
  inventory, group, and node variables, and an inventory for each
  environment.
- [Reuse work with roles](reuse-work-with-roles.md): put node tasks in a
  role, with declared inputs.
- [Preview and run changes](preview-and-run-changes.md): what each result
  and exit code means.
- Results of earlier node tasks, loops, and values that one server passes to
  another: [Use a result from one server on another](use-a-result-from-one-server-on-another.md).
- A whole example that sizes a service from facts:
  [`examples/node-facts/`](../../examples/node-facts/README.md).
