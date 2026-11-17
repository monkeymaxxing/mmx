# Reuse work with roles

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. No command on this
page asks a question, so a coding agent can run every command as written.

**When to use this page:** several playbooks or fleet stages need the same
node tasks, such as setting up the same website, or one playbook has grown
long and you want to give a group of node tasks a name and a short list of
settings.

**What this page does:** you move the nginx work of
[Write a playbook](write-a-playbook.md) into a role named `shop_website`,
with the port as a declared input, and call the role from the playbook. The
servers do not change, as `mmx dryrun` shows. Then the role learns to
run on servers with different distributions.

**Terms:**

- A **node** is a Linux server that MMX manages. A **playbook** lists
  **fleet stages**. Each fleet stage picks nodes, and each of those nodes
  runs the fleet stage's **node tasks**, in order.
- A **role** is a list of node tasks in its own file,
  `roles/<name>/main.yml`, with declared **inputs**: the values that the
  role needs, each with a type, such as `port` of type `integer`.
- A **role call** is a node task that names the role and gives values for
  the role's inputs: `role: shop_website` with `inputs: {port: 8080}`.
  The role's node tasks run where the role call stands. A role is a way to organize your files: a role does
  not run as a separate program or in another place.

| What you need | Where |
| --- | --- |
| Put node tasks in a role, and call the role | [Steps](#steps) |
| One role for Debian, Ubuntu, Fedora, and other distributions | [One role for several distributions](#one-role-for-several-distributions) |
| The input types, defaults, and required inputs | [Inputs](#inputs) |
| Template files and other files that belong to a role | [Files inside a role](#files-inside-a-role) |
| A value that the role works out, for the node tasks after the role call | [Outputs](#outputs) |
| A role that calls another role | [Role calls within roles](#role-calls-within-roles) |
| A file or a value that two roles need | [A file or a value that two roles share](#a-file-or-a-value-that-two-roles-share) |
| Roles that other people wrote | [What MMX does not have](#what-mmx-does-not-have) |

## Before you start

- `playbooks/set_up_web_servers.yml` is the playbook from
  [Write a playbook](write-a-playbook.md), and `mmx run set_up_web_servers`
  succeeds. The servers `web01` and `web02` are in the group `web_servers`
  in `inventories/example.yml`.
- The group `web_servers` sets the variable `http_port: 8080`, as in
  [Describe your servers](describe-your-servers.md), step 3. Step 6 uses
  the variable.
- Find the fields of a role call with `mmx describe-module role`.

The line numbers in the output of `mmx explain` depend on your files, so
your numbers may differ from the numbers on this page.

## Steps

1. **Create the role's file.** Create the directory `roles/shop_website/`
   and the file `roles/shop_website/main.yml`. The directory's name is the
   role's name. The file holds the node tasks of the fleet stage
   `set_up_nginx`, with three changes: the port and the page heading come
   from inputs, and the two files come from template files.

   ```yaml
   # roles/shop_website/main.yml
   inputs:
     port: {type: integer, required: true}      # an input name you choose
     heading: {type: string, default: Shop}

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
         template: files/index.html.tmpl        # roles/shop_website/files/index.html.tmpl

     - task_block:
         id: shop_site
         changes:
           - id: write_site_file
             file:
               path: /etc/nginx/conf.d/shop.conf
               permissions_mode: "0644"
               template: files/shop.conf.tmpl   # roles/shop_website/files/shop.conf.tmpl
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
               url_responds: "http://127.0.0.1:{{ .inputs.port }}/"
               response_body_contains: "{{ .inputs.heading }}"
   ```

   `inputs` declares the role's inputs. `port` is a whole number, and each
   role call must give `port`. `heading` is text, and a role call that
   leaves out `heading` gets `Shop`. Node tasks in the role read an input
   as `{{ .inputs.port }}`.

   **Check:** none yet: `mmx validate` reads a role only when a playbook
   calls the role, in step 3.

2. **Create the role's template files.** Inside a role, a `template` path
   starts at the role's directory, so `files/index.html.tmpl` is
   `roles/shop_website/files/index.html.tmpl`. Create
   `roles/shop_website/files/index.html.tmpl`:

   ```text
   <h1>{{ .inputs.heading }}</h1>
   ```

   Create `roles/shop_website/files/shop.conf.tmpl`:

   ```text
   server {
     listen {{ .inputs.port }};
     root /srv/shop/public;
   }
   ```

   A template in a role reads the role's inputs, as the role's node tasks
   do. The role's directory now holds:

   ```text
   roles/shop_website/files/index.html.tmpl
   roles/shop_website/files/shop.conf.tmpl
   roles/shop_website/main.yml
   ```

   **Check:** none yet: step 3 checks the files.

3. **Call the role from the playbook.** Replace the contents of
   `playbooks/set_up_web_servers.yml`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   fleet_stages:
     - id: set_up_nginx
       nodes: web_servers
       node_tasks:
         - id: shop_website          # a node task ID you choose
           role: shop_website        # the directory name under roles/
           inputs:
             port: 8080
   ```

   The role file's `inputs` declares which inputs exist. The role call's
   `inputs` gives their values.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:` and the
   project's name. If the role call leaves out `port`, the message is
   `role shop_website: missing input port`. See
   [If something is different](#if-something-is-different) for the other
   messages.

4. **See what the role call gives each server.**

   ```sh
   mmx explain set_up_web_servers --limit web01
   ```

   **Check:** the role call lists each input's value and where the value
   came from: the role call, or the role's `default`. Then each node task
   of the role follows, with the role's file and line:

   ```text
   set_up_nginx → web01
     web01 / set_up_nginx/shop_website: role (ready)
       at playbooks/set_up_web_servers.yml:5:9 (fleet_stages[0].node_tasks[0])
       inputs.heading = "Shop"
         from roles/shop_website/main.yml:3:36 (inputs.heading.default)
       inputs.port = 8080
         from playbooks/set_up_web_servers.yml:8:17 (fleet_stages[0].node_tasks[0].inputs.port)
     web01 / set_up_nginx/shop_website/install_nginx: apt_package (ready)
       at roles/shop_website/main.yml:6:5 (node_tasks[0])
   ```

5. **Preview the playbook.**

   ```sh
   mmx dryrun set_up_web_servers --limit web01
   echo "exit code $?"
   ```

   **Check:** every node task says `unchanged`, and the exit code is 0.
   The role writes the same files that the playbook wrote before, so
   moving work into a role changes nothing on the servers. The output
   names each node task with the role call's ID first:

   ```text
   set_up_nginx / web01: succeeded
     set_up_nginx / web01 / shop_website/install_nginx: succeeded, unchanged
     set_up_nginx / web01 / shop_website/create_site_directory: succeeded, unchanged
     set_up_nginx / web01 / shop_website/create_site_directory: directory "/srv/shop/public"
       checked 1 directories and 0 files; 0 metadata updates
     set_up_nginx / web01 / shop_website/write_home_page: succeeded, unchanged
     set_up_nginx / web01 / shop_website/shop_site/changes/write_site_file: succeeded, unchanged
     set_up_nginx / web01 / shop_website/shop_site/activate/reload_nginx: succeeded, unchanged
     set_up_nginx / web01 / shop_website/shop_site/verify/check_home_page: succeeded, unchanged
     set_up_nginx / web01 / shop_website/shop_site: succeeded, unchanged
     set_up_nginx / web01 / shop_website: succeeded, unchanged
   Run b72f70dd: succeeded
   ```

   If a node task says `would_change`, the role's file differs from the
   old playbook's file. `mmx dryrun set_up_web_servers --diff` shows the
   difference. A role call without an `id` uses the role's name in the
   output.

6. **Give the role the port from a variable.** A role reads no variable
   of the project, the inventory, a group, a node, or the command line, so
   the playbook passes the variable to the role as an input. In
   `playbooks/set_up_web_servers.yml`, change the `port` line:

   ```yaml
           inputs:
             port: "{{ .vars.http_port }}"
   ```

   `port` keeps its type, a whole number, because the value is exactly one
   reference.

   **Check:** `mmx explain set_up_web_servers --limit web01` shows the
   value and the inventory line that set the value:

   ```text
       inputs.port = 8080
         from playbooks/set_up_web_servers.yml:8:17 (fleet_stages[0].node_tasks[0].inputs.port)
         value from inventories/example.yml:16:18 (groups.web_servers.vars.http_port)
   ```

   Then `mmx run set_up_web_servers` says `unchanged` for every node task
   and ends with `Run ...: succeeded`.

## One role for several distributions

MMX has a module for each package manager, such as `apt_package` for apt on
Debian and Ubuntu and `dnf_package` for dnf on Fedora, Rocky Linux, and
AlmaLinux. MMX does not choose the module for you. A role that runs on
several distributions has one node task for each package manager, each
with a `when` on the fact `.node.package_manager`. A **fact** is a value
that the node reports about itself. These steps continue from step 6 and
add `web03`, an Ubuntu 26.04 server at `203.0.113.13`.

1. **Add the server.** In `inventories/example.yml`, add `web03` below
   `web02`, and add `web03` to the group:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root
     web02:
       address: 203.0.113.12
       ssh_user: root
     web03:
       address: 203.0.113.13
       ssh_user: root

   groups:
     web_servers:
       nodes: [web01, web02, web03]
       vars:
         http_port: 8080
   ```

   **Check:** `mmx nodes` lists `web03` in `web_servers`. Your computer
   must trust `web03`'s SSH host key, as
   [Connect to your servers](connect-to-your-servers.md) explains.

2. **Give each package manager its own node task.** In
   `roles/shop_website/main.yml`, replace `install_nginx` with two node
   tasks:

   ```yaml
     - id: install_nginx_with_apt
       when:
         equals: ["{{ .node.package_manager }}", apt]
       apt_package:
         packages:
           - {name: nginx, version: newest_when_first_installed}

     - id: install_nginx_with_dnf
       when:
         equals: ["{{ .node.package_manager }}", dnf]
       dnf_package:
         packages:
           - {name: nginx, version: newest_when_first_installed}
   ```

   Each node runs the node task for its own package manager, and skips the
   other node task.

3. **Look up a value that differs by distribution.** nginx's workers run
   as the account `www-data` on Debian and Ubuntu, and as `nginx` on
   Fedora and Rocky Linux. A mapping input holds both names, and a node
   task looks up its own. Add the input below `heading`:

   ```yaml
     nginx_account:                             # the account nginx's workers run as
       type: mapping
       default: {apt: www-data, dnf: nginx}
   ```

   Add a directory that nginx's workers can write to, below
   `create_site_directory`:

   ```yaml
     - id: create_upload_directory
       directory:
         path: /srv/shop/uploads
         owner: "{{ .inputs.nginx_account[.node.package_manager] }}"
   ```

   `[.node.package_manager]` looks up the key `apt` or `dnf` in the
   mapping. A missing key is an error that stops the node task, never an
   empty value.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:`.

4. **Preview and run.**

   ```sh
   mmx dryrun set_up_web_servers
   mmx run set_up_web_servers
   ```

   **Check:** on every server, `install_nginx_with_dnf` says
   `skipped, unchanged — when is false`, because Debian and Ubuntu use
   apt. On `web03`, the first run installs nginx:

   ```text
     set_up_nginx / web03 / shop_website/install_nginx_with_apt: succeeded, changed
       [{"name":"nginx-common","to":"1.28.3-2ubuntu1.12"},{"name":"nginx","to":"1.28.3-2ubuntu1.12"}]
     set_up_nginx / web03 / shop_website/install_nginx_with_dnf: skipped, unchanged — when is false
   ```

   On the first run on `web03`, `reload_nginx` says
   `succeeded, changed — service was not running; started`, because MMX installs a
   package without starting its services, so MMX starts nginx
   ([Reload or restart](change-a-configuration-and-reload-its-service.md#reload-or-restart)).
   On `web01`
   and `web02`, only `create_upload_directory` changes. The run ends with
   `Run ...: succeeded`, and on each server `/srv/shop/uploads` belongs to
   `www-data`.

The dryrun's exit code is 3 on a run that adds `web03`: on a server without
nginx, a dryrun cannot check the site file in `/etc/nginx/conf.d`, which
the nginx package creates.
[Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
explains `not_checked`.

[Use variables and templates](use-variables-and-templates.md#node-facts)
lists the other facts about the distribution, such as
`.node.distribution.id` and `.node.distribution.family`. A package node task on a server with another package manager
fails, and the message says what to do:

```text
dnf_package installs with dnf, but this node's package manager is apt (the node fact package_manager); give the node task a condition so that it runs only on nodes that use dnf
```

The module pages say how each package module installs, upgrades, and
removes packages, and how to add a package source, such as
[`apt_package`](../reference/modules/apt_package.md),
[`dnf_package`](../reference/modules/dnf_package.md),
[`apt_repository`](../reference/modules/apt_repository.md), and
[`dnf_repository`](../reference/modules/dnf_repository.md). Arch Linux and openSUSE
use the official extra modules `pacman_package` and `zypper_package`;
`mmx describe-module pacman_package` shows the inputs.
[Limits on each distribution](../reference/limits-on-each-distribution.md) says what
is limited where.

## The whole role

`roles/shop_website/main.yml` after the steps:

```yaml
# roles/shop_website/main.yml
inputs:
  port: {type: integer, required: true}      # an input name you choose
  heading: {type: string, default: Shop}
  nginx_account:                             # the account nginx's workers run as
    type: mapping
    default: {apt: www-data, dnf: nginx}

node_tasks:
  - id: install_nginx_with_apt
    when:
      equals: ["{{ .node.package_manager }}", apt]
    apt_package:
      packages:
        - {name: nginx, version: newest_when_first_installed}

  - id: install_nginx_with_dnf
    when:
      equals: ["{{ .node.package_manager }}", dnf]
    dnf_package:
      packages:
        - {name: nginx, version: newest_when_first_installed}

  - id: create_site_directory
    directory:
      path: /srv/shop/public
      create_missing_parent_directories: true

  - id: create_upload_directory
    directory:
      path: /srv/shop/uploads
      owner: "{{ .inputs.nginx_account[.node.package_manager] }}"

  - id: write_home_page
    file:
      path: /srv/shop/public/index.html
      permissions_mode: "0644"
      template: files/index.html.tmpl        # roles/shop_website/files/index.html.tmpl

  - task_block:
      id: shop_site
      changes:
        - id: write_site_file
          file:
            path: /etc/nginx/conf.d/shop.conf
            permissions_mode: "0644"
            template: files/shop.conf.tmpl   # roles/shop_website/files/shop.conf.tmpl
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
            url_responds: "http://127.0.0.1:{{ .inputs.port }}/"
            response_body_contains: "{{ .inputs.heading }}"
```

`playbooks/set_up_web_servers.yml`:

```yaml
# playbooks/set_up_web_servers.yml
fleet_stages:
  - id: set_up_nginx
    nodes: web_servers
    node_tasks:
      - id: shop_website          # a node task ID you choose
        role: shop_website        # the directory name under roles/
        inputs:
          port: "{{ .vars.http_port }}"
```

## Inputs

Each input under the role file's `inputs` has a `type`. Give an input
`required: true` when each role call must give the input, or a `default`
for a role call that leaves the input out:

| `type` | A value such as |
| --- | --- |
| `string` | `Shop` |
| `integer` | `8080` |
| `boolean` | `true` or `false` |
| `list` | `[alice, bob]` |
| `mapping` | `{apt: www-data, dnf: nginx}` |

- A role call must give each input that has `required: true`. A role call
  may leave out an input that has a `default`.
- An input without `required` and without a `default` has no value when the
  role call leaves the input out. A node task that reads the input then
  fails with `value is unavailable`.
- A `default` is a value as written, never a reference. A role call passes
  a variable to the role, as in step 6. `default: "{{ .vars.site_name }}"`
  fails with
  `role shop_website: input heading default must be a literal value; pass references through the role call's inputs mapping`.
- MMX checks a written value's type when MMX reads the playbook:
  `port: "8080"` is text, and fails with `input port must be integer`. A
  value from a reference is checked where the reference gets its value;
  [A number, a boolean, a list, or a mapping from a reference](use-variables-and-templates.md#a-number-a-boolean-a-list-or-a-mapping-from-a-reference)
  says where.
- An input with `secret: true` holds a secret, such as a password, and
  takes no `default`. Declare `secret: true` on an input that receives a
  secret; [Secrets in files, output, and history](../reference/secrets.md#secrets-in-files-output-and-history)
  gives the error of a role call that does not. [Keep secrets](keep-secrets.md)
  explains secrets.
- A role call's `inputs` can read the same values as any node task in its
  fleet stage: variables, facts, `{{ .secrets.payment_key }}`, and an
  earlier fleet stage's output, such as
  `{{ .outputs.prepare_database.db01.api_token }}`
  ([Use a result from one server on another](use-a-result-from-one-server-on-another.md)).

## Files inside a role

Inside a role, a relative path in `template` and `source`, and in the
other fields that name files on your computer, starts at the role's
directory. Keep a role's files in the role's directory, such as
`roles/shop_website/files/`, so that the role works when you copy the
directory. A relative path may leave the role's directory but must stay
inside the project, and an absolute path may name any file on your
computer. [The `file` module page](../reference/modules/file.md#paths-on-the-controller-machine)
lists the fields that name files on your computer and says more.

## What a role reads

A role reads its inputs, its node's facts (`.node`), the results of its
own earlier node tasks (`.node_tasks`), and the variables that its own
`set_vars` node tasks set. A role reads a secret only through an input
with `secret: true`. The variables of the project, the inventory, the
groups, the nodes, and the command line reach a role only through the role
call's inputs. `{{ .vars.http_port }}` inside a role fails
with
`reference .vars.http_port is not available in a role: a role reads only its inputs and the variables its own set_vars set earlier; declare an input in the role file and pass the value in the role call's inputs`.
The reason: a role that reads only its inputs works the same in each
playbook and each project, and `mmx explain` shows every value that the
role gets at the role call.

## Outputs

A role can give values back to the node tasks after the role call. The
role declares the values in `outputs`, beside `inputs`:

```yaml
# roles/shop_website/main.yml, below inputs
outputs:
  home_page_url: "http://127.0.0.1:{{ .inputs.port }}/"
```

A later node task in the same fleet stage reads the value through the role
call's ID:

```yaml
      - show: "Home page: {{ .node_tasks.shop_website.outputs.home_page_url }}"
```

The run prints `"Home page: http://127.0.0.1:8080/"`. MMX works out the
outputs after the role's node tasks succeed. The rest of the playbook sees
only the declared outputs, not the role's own node tasks.

## Role calls within roles

By default, a role cannot call another role. The playbook then lists every
role call, in the order that the role calls run, so you read the whole
order in one file. MMX does not work out an order between roles: put a
role that another role needs first in the playbook. A role call inside a
role fails with:

```text
role calls within roles require allow_role_calls_within_roles: true
```

To let roles call roles, add the setting to `mmx_project_settings.yml`:

```yaml
# mmx_project_settings.yml
allow_role_calls_within_roles: true
```

The output then names each node task with each role call's name, such as
`shop_website/nginx/install_nginx_with_apt` for a role `nginx` that
`shop_website` calls. A role that calls itself, directly or through
another role, is an error even with the setting:
`recursive role call: shop_website`.

## A file or a value that two roles share

| Situation | What to do |
| --- | --- |
| Two kinds of server need the same setup, and neither server's role owns the setup | Put the shared setup in a role of its own. Call the shared role from one fleet stage that selects both groups, such as `nodes: [web_servers, database_servers]`. A node in both groups runs the shared role once. |
| Two roles are the same role for two environments | Keep one role. Each environment's inventory sets the values that differ, and the playbook passes the values to the role as inputs. |
| Two roles read the same value, such as a list of Redis instances | Set the value once in the project's or the inventory's `vars`. The playbook passes the value to each role as an input. |
| One role needs a value that only another role's work finds out | The first role declares an output, and the playbook passes the output to the second role as an input. |

## What MMX does not have

MMX has no shared collection of roles to download, and no search path for
roles. A role is a directory in your project's `roles/`, written for your
project. To use a role in a second project, copy the role's directory to
the second project's `roles/`. The modules are the shared part: the core
modules, such as `apt_package` and `file`, come with MMX, and
`mmx describe-module` lists every module.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `role shop_website: missing input port` | The role call leaves out an input with `required: true`. Add the input under the role call's `inputs`. |
| `role shop_website: unknown input title` | The role call gives an input that the role file does not declare. Check the name, or declare the input in the role file. |
| `input port must be integer` | A written value has the wrong type, such as `port: "8080"`, which is text. Write `port: 8080`. |
| `open /home/you/shop/roles/shop_site/main.yml: no such file or directory` | No role has this name. A role's name is its directory under `roles/`. |
| `role shop_website: input heading needs type string, integer, boolean, list, or mapping` | The input's `type` is not one of the five types. |
| `reference .vars.http_port is not available in a role: ...` | A role reads `.vars`. Declare an input, and pass the variable in the role call, as in step 6. |
| `invalid reference ".inputs.heading \| title"` | A function call in the role's YAML. Use the value as it is, or move the text to a template file; [Functions in templates](use-variables-and-templates.md#functions-in-templates) explains why. |
| `role calls within roles require allow_role_calls_within_roles: true` | See [Role calls within roles](#role-calls-within-roles). |
| `dnf_package installs with dnf, but this node's package manager is apt` | A package node task runs on a server with another package manager. Add a `when`, as in [One role for several distributions](#one-role-for-several-distributions). |

## Where to go next

- [Use variables and templates](use-variables-and-templates.md): the
  values a role call can pass, facts, and functions in templates.
- [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
  node task blocks in full, inside a role or outside one.
- A small whole project with one role that runs on apt and dnf servers:
  [`examples/nginx-role/`](../../examples/nginx-role/).
- How roles fit with the other parts of a playbook:
  [How MMX works](../how-it-works.md#your-project-describes-your-servers).
