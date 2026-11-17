# Move from Ansible or Salt

**Who acts:** you, or your coding agent, on your controller machine: the
computer where you run `mmx`, usually your laptop. No command on this page
asks a question when MMX logs in as `root` with a key. With a `sudo`
password, a coding agent adds `--non-interactive` and gives the password as
[Passwords for automation](connect-to-your-servers.md#passwords-for-automation)
shows.

**When to use this page:** you have an [Ansible](https://docs.ansible.com/)
playbook or a [Salt](https://docs.saltproject.io/) state, and you want the
MMX way to do the same work.

**What this page does:** a table gives the MMX counterpart of each Ansible
and Salt idea, with a link to the page that explains the MMX side. Then
you convert a small Ansible playbook to MMX, step by step: nginx with a
template, a handler, a group, and a variable for the group. Last, tables
map Ansible's commands, task keywords, and modules to MMX.

**Terms:**

- A **node** is a Linux server that MMX manages. An **inventory** is a
  file in `inventories/` that lists the nodes, how to reach each node, and
  the groups of nodes.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**, run
  in order. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order.
- A node task names one **module**, the code that does the work, such as
  `apt_package` or `file`, with the module's inputs below its name.
- A **node task block** groups a change with what follows it on the same
  node: making the change, putting the change into use, and checking the
  change.

| What you need | Where |
| --- | --- |
| The MMX counterpart of an Ansible or Salt idea, such as handlers or pillar | [Ansible and Salt ideas in MMX](#ansible-and-salt-ideas-in-mmx) |
| A worked conversion of a playbook, run on real servers | [Convert a playbook](#convert-a-playbook) |
| The MMX command for an `ansible-playbook` or `salt` command | [Commands](#commands) |
| The MMX form of a task keyword, such as `register` or `become` | [Task keywords](#task-keywords) |
| The MMX module for an Ansible module or a Salt state function | [Modules](#modules) |

## Before you start

- `mmx` is installed on your controller machine, as
  [Get started](../get-started.md) step 1 shows.
- Your controller machine trusts each server's SSH host key, as Get started
  step 2 shows. MMX logs in as `root`, or as an account that can run any
  command with `sudo`.
- For the worked conversion: two servers that run Debian or Ubuntu,
  because the playbook installs nginx with `apt_package`. The examples use
  `web01` at `203.0.113.11` and `web02` at `203.0.113.12`, both Debian 13.
- MMX manages only Linux servers that run
  [systemd](https://systemd.io/). For Windows servers, network devices, or
  Linux without systemd, keep Ansible or Salt for those machines.

## Ansible and Salt ideas in MMX

Each row names the Ansible idea, the Salt idea, and what MMX uses instead.
The link in each row goes to the page that explains the MMX side.

| Ansible / Salt | In MMX |
| --- | --- |
| **Inventory** (Ansible) / **minion targeting, nodegroups** (Salt) | An inventory file, `inventories/<name>.yml`, lists each node with its `address` and `ssh_user`, and the groups. A command chooses an inventory with `--inventory <name>`. A fleet stage selects nodes by inventory node and group names only, not by facts or name patterns: `--limit web01,web02` replaces Salt's `-L`, there is no pattern algebra such as `web:&prod` or `!web03`, and environments are separate inventories. [Describe your servers](describe-your-servers.md), [Choose which servers a run touches](choose-which-servers-a-run-touches.md) |
| **`group_vars`, `host_vars`** (Ansible) / **pillar** (Salt) | `vars` in four places: the project settings file `mmx_project_settings.yml`, the top of the inventory, a group, and a node. A later place replaces an earlier one, in that order. When two groups of one node set the same variable, MMX refuses the inventory instead of choosing a group. For secret pillar values, see the vault row below. [Describe your servers](describe-your-servers.md#steps) step 3, [Where a value comes from](use-variables-and-templates.md#where-a-value-comes-from) |
| **Playbook, play** (Ansible) / **top file, highstate** (Salt) | A playbook, `playbooks/<name>.yml`, applied with `mmx run <name>`. MMX has no default playbook, so each command names one. An Ansible play becomes a fleet stage: `nodes` in place of `hosts`, and `node_tasks` in place of `tasks`. [Write a playbook](write-a-playbook.md) |
| **Task** (Ansible) / **state, state function** such as `pkg.installed` (Salt) | A node task: an `id` you choose and one module with its inputs. Each module is named for its tool, such as `apt_package` for apt and `dnf_package` for dnf. MMX has no module that picks the tool for you, such as Ansible's `package`. [Find the module for a job](find-the-module-for-a-job.md), [Modules](#modules) below |
| **Handlers, `notify`** (Ansible) / **`watch`, `onchanges`** (Salt) | A node task block. The `changes` phase writes files, and the `activate` phase reloads or restarts the service when a file in `changes` differs from what the service last loaded. MMX keeps that record on the node, so the next run still reloads after a run that stopped between the write and the reload. [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md), [A run that stopped before the reload](change-a-configuration-and-reload-its-service.md#a-run-that-stopped-before-the-reload) |
| **`block`, `rescue`, `always`** (Ansible) / **`onfail`** (Salt) | A node task block's `on_failure` and `cleanup` phases. `on_failure` runs only when an earlier phase of the block failed, and `cleanup` runs last, after success or failure. Unlike a successful Ansible `rescue`, a successful `on_failure` does not clear the failure: the node task block still fails. [The phases in order](change-a-configuration-and-reload-its-service.md#the-phases-in-order) |
| **Roles** (Ansible) / **formulas, SLS files** (Salt) | A role: a file `roles/<name>/main.yml` with declared inputs and node tasks, called from a node task with `role:` and `inputs:`. Ansible's `include_tasks` and `import_tasks` also become roles. [Reuse work with roles](reuse-work-with-roles.md) |
| **Ansible Vault** / **secrets in pillar** | The vault: one encrypted file of secrets in the project, edited with `mmx secrets edit`. A node task reads a secret as `{{ .secrets.<key> }}`, and MMX hides the value in output and saved runs. A node receives only the secrets that its own node tasks use. [Keep secrets](keep-secrets.md), [Secrets reference](../reference/secrets.md) |
| **Facts, `gather_facts`** (Ansible) / **grains** (Salt) | Facts under `.node`, such as `.node.distribution.id` and `.node.memory.total_mib`. MMX collects the facts each time a node starts a fleet stage, so a playbook has no setting for gathering. A fleet stage cannot select nodes by a fact, as Salt's grain targeting does: put the nodes in a group, or use a `when` condition on a fact. [Node facts](use-variables-and-templates.md#node-facts) |
| **`ansible_host`, `ansible_user`, `ansible_port`, `ansible_ssh_private_key_file`, SSH and sudo passwords** (Ansible) | Node fields `address`, `ssh_user`, `ssh_port`, and `ssh_identity_file`. The fields `ssh_known_hosts` and `ssh_config` name SSH client files for the node. A password is a secret reference with no text around it: `ssh_password: "{{ .secrets.ssh_password }}"` and `sudo_password: "{{ .secrets.sudo_password }}"`. MMX keeps a password only in the memory of the `mmx` process on the controller machine, and never writes it to the inventory, to the node, or to a result. `connection: ssh_only` skips HTTPS, and `https_address` sets the HTTPS address when it is not `address` with port 7443. [Inventory file](../reference/inventory-file.md), [Connect to your servers](connect-to-your-servers.md) |
| **Jinja2 templates and filters** (Ansible) | Go `text/template` files, with the functions of the Sprig library and `toYaml`. MMX leaves out the Sprig functions that read the clock, random values, the environment, or the network, so that a run with nothing new reports `unchanged`. [Functions in templates](use-variables-and-templates.md#functions-in-templates) |
| **`hostvars[other]`, `groups['web']`** (Ansible) / **Salt mine** | `.inventory.nodes.NAME` and `.inventory.groups.NAME` for the inventory, and fleet stage outputs for values a fleet stage found out. [Values about other servers](use-variables-and-templates.md#values-about-other-servers) |
| **A role or `vars/{{ ansible_os_family }}.yml` for every distribution** (Ansible) | MMX has no module that picks the tool for the distribution. A mixed fleet uses a `when` on `.node.distribution.id` or `.node.distribution.family` to choose between node tasks that name their tool, such as `apt_package` and `dnf_package`. [One role for several distributions](reuse-work-with-roles.md#one-role-for-several-distributions) |
| **`serial`, `forks`** (Ansible) / **`--batch-size`** (Salt) | `max_nodes_at_once` on a fleet stage: a number or a percentage. `wait_for_all_in_batch: true` makes each batch finish before the next one starts. [Roll out a change gradually](roll-out-a-change-gradually.md) |
| **`delegate_to`** (Ansible) | MMX has no counterpart keyword, because each fleet stage names the nodes that do the fleet stage's work. Write the other node's work as its own fleet stage on that node. For work on another node for each node in turn, such as taking each web server out of a load balancer, use a `for_each_node` loop. [Do one server's work on another server](use-a-result-from-one-server-on-another.md#do-one-servers-work-on-another-server), [Take each server out of the load balancer while it changes](roll-out-a-change-gradually.md#take-each-server-out-of-the-load-balancer-while-it-changes) |
| **`run_once`** (Ansible) | A fleet stage whose `nodes` names one node, such as `nodes: db01`, for work such as a database migration. A later fleet stage can wait for that work with `requires`. [Run a fleet stage only where an earlier one succeeded](roll-out-a-change-gradually.md#run-a-fleet-stage-only-where-an-earlier-one-succeeded) |
| **Tags, `--tags`, `--skip-tags`, `--start-at-task`** (Ansible) | MMX has no tags and no option to start at a node task, because tags and a start task can skip work that a later step needs. Keep each part that you apply on its own in its own fleet stage, and choose fleet stages with `--only-fleet-stages`. MMX refuses to leave out a fleet stage that a kept one needs. [Run only some fleet stages](choose-which-servers-a-run-touches.md#run-only-some-fleet-stages) |
| **Check mode, `--check --diff`** (Ansible) / **`test=True`** (Salt) | `mmx dryrun <playbook>`, with `--diff` for the lines of each file that would change. The exit code says whether anything would change. [Preview and run changes](preview-and-run-changes.md) |
| **Ad hoc commands, `ansible all -m shell -a '...'`** (Ansible) / **`salt '*' cmd.run '...'`** (Salt) | `mmx run-command --nodes all -- <command>`. [Run a command on servers now](run-a-command-on-servers-now.md) |
| **Ansible Galaxy, collections** / **Salt formulas from `saltstack-formulas`** | MMX has no counterpart: no shared collection of roles to download, and no search path for roles. MMX has no shared collection of roles: a project's roles are written for that project. To reuse a role, copy the role's directory into the other project's `roles/`. The shared parts are the core modules, which come with MMX, and the official extra modules, which come in the same release archive. [What MMX does not have](reuse-work-with-roles.md#what-mmx-does-not-have), [Official extra modules](find-the-module-for-a-job.md#official-extra-modules) |
| **Salt master and minions, `salt-key -A`** | MMX has no master and no key acceptance. `mmx-runner`, MMX's node program, resembles a minion: it runs as root on the node, runs jobs there, and keeps their results. The first `mmx dryrun` or `mmx run` on a server logs in over SSH and installs MMX's node program, `mmx-runner`. Your trusted SSH host keys and your SSH login decide which servers and people MMX trusts. [What the first dryrun and the first run did on the server](../get-started.md#what-the-first-dryrun-and-the-first-run-did-on-the-server) |
| **Salt mine** | A fleet stage output: a value that a fleet stage publishes from each of its nodes, which later fleet stages read. [Use a result from one server on another](use-a-result-from-one-server-on-another.md) |
| **Salt reactor, beacons, scheduler** / **`ansible-pull`, AWX schedules** | MMX works only when you or a program run an `mmx` command. To apply a playbook on a schedule, run `mmx run` from CI or from a systemd timer on a controller machine. For health checks and automatic restarts, use a monitoring tool. [Run the project from CI](move-or-share-a-project.md#run-the-project-from-ci) |

## Convert a playbook

The Ansible project below installs nginx on the group `web_servers`. A
template writes an nginx site whose port comes from a variable of the
group, and a handler reloads nginx when the site file changed:

```yaml
# inventory.yml
web_servers:
  hosts:
    web01:
      ansible_host: 203.0.113.11
      ansible_user: root
    web02:
      ansible_host: 203.0.113.12
      ansible_user: root
```

```yaml
# group_vars/web_servers.yml
http_port: 8080
```

```text
# templates/shop.conf.j2
server {
  listen {{ http_port }};
  default_type text/plain;
  return 200 "Shop on {{ inventory_hostname }}\n";
}
```

```yaml
# site.yml
- name: Set up nginx
  hosts: web_servers
  tasks:
    - name: Install nginx
      ansible.builtin.apt:
        name: nginx
        state: present
        update_cache: true

    - name: Write the shop site
      ansible.builtin.template:
        src: templates/shop.conf.j2
        dest: /etc/nginx/conf.d/shop.conf
        mode: "0644"
      notify: Reload nginx

    - name: Start nginx
      ansible.builtin.service:
        name: nginx
        state: started
        enabled: true

  handlers:
    - name: Reload nginx
      ansible.builtin.service:
        name: nginx
        state: reloaded
```

The steps write the same work as an MMX project and run it on two fresh
Debian 13 servers.

### Steps

1. **Create an MMX project.** In a new, empty directory beside the Ansible
   project:

   ```sh
   mkdir shop && cd shop
   mmx init
   rm playbooks/example.yml
   ```

   `mmx init` writes an example playbook, `playbooks/example.yml`, that
   this conversion does not need.

   **Check:** `mmx init` prints:

   ```text
   Created mmx_project_settings.yml, inventories/example.yml, and playbooks/example.yml. Set the SSH destination in inventories/example.yml, then run mmx dryrun example.
   ```

2. **Convert the inventory and `group_vars`.** Replace the contents of
   `inventories/example.yml`. The group's variables go into the inventory
   file, under the group:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11    # ansible_host
       ssh_user: root           # ansible_user
     web02:
       address: 203.0.113.12
       ssh_user: root

   groups:
     web_servers:
       nodes: [web01, web02]
       vars:
         http_port: 8080        # group_vars/web_servers.yml
   ```

   Every key of a `group_vars` file moves into the `vars` of that group,
   so a second key such as `site_title: Shop` becomes a second line below
   `http_port`. Variables from `group_vars/all.yml` go into `vars` at the
   top of the inventory file, and variables from `host_vars/web01.yml` go
   into `vars` under the node `web01`.

   An INI inventory maps in the same way. Each `[web_servers]` section
   becomes an entry under `groups:` with a `nodes:` list. Each host line
   becomes an entry under `nodes:`, with `ansible_host` as `address`,
   `ansible_user` as `ssh_user`, and `ansible_port` as `ssh_port`. A
   variable written on a host line, such as `web01 http_port=8081`, goes
   into `vars` under the node `web01`. A `[web_servers:vars]` section goes
   into `vars` under the group.

   **Check:** `mmx nodes` lists both servers in the group. `mmx nodes`
   contacts no server.

   ```text
   Inventory: example (inventories/example.yml, 2 nodes)

   Nodes
     web01   203.0.113.11   root   web_servers
     web02   203.0.113.12   root   web_servers

   Groups
     web_servers   web01, web02
   ```

   If `mmx nodes` names a field that is wrong, compare the field with
   [The inventory file reference](../reference/inventory-file.md).

3. **Convert the template.** Create `files/shop.conf.tmpl`. MMX templates
   use Go's [`text/template`](https://pkg.go.dev/text/template) syntax
   instead of Jinja2, so each reference changes:

   ```text
   server {
     listen {{ .vars.http_port }};
     default_type text/plain;
     return 200 "Shop on {{ .node.name }}\n";
   }
   ```

   | Jinja2 in Ansible | MMX template |
   | --- | --- |
   | `{{ http_port }}`, a variable | `{{ .vars.http_port }}` |
   | `{{ inventory_hostname }}`, the server's name in the inventory | `{{ .node.name }}` |
   | `{{ ansible_facts['distribution'] }}`, a fact | `{{ .node.distribution.id }}` |
   | A filter, such as `{{ name \| title }}` | A function, such as `{{ .vars.name \| title }}`, from the list in [Functions in templates](use-variables-and-templates.md#functions-in-templates) |

   **Check:** step 6 shows the template filled in for `web01`. A reference
   to a variable that the node does not get is an error there, not empty
   text.

4. **Convert the play and the package task.** Create
   `playbooks/set_up_web_servers.yml`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   fleet_stages:
     - id: set_up_nginx            # the play
       nodes: web_servers          # hosts: web_servers
       node_tasks:
         - id: install_nginx       # the task "Install nginx"
           apt_package:
             packages:
               - {name: nginx, version: newest_when_first_installed}
   ```

   Three settings of the Ansible task have no line in the MMX file:

   - `state: present` becomes `version: newest_when_first_installed`. MMX
     installs the newest version when nginx is missing, and then leaves the
     installed version alone.
   - `update_cache: true` has no counterpart, because `apt_package`
     refreshes the package index itself before an installation.
   - Ansible's `become: true` runs a task as root through sudo. MMX runs
     each node task as root, so MMX has no `become`.

   **Check:** `mmx validate set_up_web_servers` prints
   `Valid: mmx_example`.

5. **Convert the template task, the service task, and the handler into a
   node task block.** Add this below `install_nginx`, at the same indent:

   ```yaml
         - task_block:
             id: shop_site
             changes:                  # the task "Write the shop site"
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   template: files/shop.conf.tmpl
             activate:                 # the task "Start nginx" and the handler "Reload nginx"
               - id: reload_nginx
                 systemd_service:
                   name: nginx
                   start_at_boot: true   # enabled: true
                   state: running        # state: started
                   refresh: reload       # state: reloaded, when the site file changed
             verify:                   # new: check that the site answers
               - id: check_shop_site
                 wait_for:
                   url_responds: "http://127.0.0.1:{{ .vars.http_port }}/"
                   response_body_contains: Shop on
   ```

   Reason: Ansible's handler runs at the end of the play, and only when
   the template task reported a change in this run. In MMX, the
   `activate` phase runs inside the node task block, after the `changes`
   phase, on the same node. `systemd_service` starts nginx when nginx is
   stopped. `systemd_service` reloads nginx when the site file
   differs from the file that nginx last loaded, also when an earlier run
   wrote the file and then stopped before the reload. `verify` asks the
   server itself for the page and fails the node task block when the
   answer does not contain `Shop on`.

   **Check:** `mmx validate set_up_web_servers` prints
   `Valid: mmx_example`. The whole file now reads:

   ```yaml
   # playbooks/set_up_web_servers.yml
   fleet_stages:
     - id: set_up_nginx
       nodes: web_servers
       node_tasks:
         - id: install_nginx
           apt_package:
             packages:
               - {name: nginx, version: newest_when_first_installed}

         - task_block:
             id: shop_site
             changes:
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   template: files/shop.conf.tmpl
             activate:
               - id: reload_nginx
                 systemd_service:
                   name: nginx
                   start_at_boot: true
                   state: running
                   refresh: reload
             verify:
               - id: check_shop_site
                 wait_for:
                   url_responds: "http://127.0.0.1:{{ .vars.http_port }}/"
                   response_body_contains: Shop on
   ```

6. **See what one server will get.**

   ```sh
   mmx explain set_up_web_servers --limit web01
   ```

   `mmx explain` contacts no server. `mmx explain` prints each node task's values and
   the line of your files that each value came from.

   **Check:** the site file appears filled in for `web01`, and the URL of
   the check shows the inventory line of `http_port`:

   ```text
     web01 / set_up_nginx/shop_site/changes/write_site_file: file (ready)
       ...
       file.template, rendered for web01:
         server {
           listen 8080;
           default_type text/plain;
           return 200 "Shop on web01\n";
         }
     ...
     web01 / set_up_nginx/shop_site/verify/check_shop_site: wait_for (ready)
       ...
       wait_for.url_responds = "http://127.0.0.1:8080/"
         from playbooks/set_up_web_servers.yml:29:31 (fleet_stages[0].node_tasks[1].task_block.verify[0].wait_for.url_responds)
         value from inventories/example.yml:14:18 (groups.web_servers.vars.http_port)
   ```

   If `mmx explain` says that a reference names a value that is not
   declared, check the spelling of the variable in the template and in
   the inventory.

7. **Preview the playbook.**

   ```sh
   mmx dryrun set_up_web_servers
   echo "exit code $?"
   ```

   The first dryrun on each server also sets up MMX there, over SSH. Part
   of the output:

   ```text
   203.0.113.12: setting up MMX through verified SSH
   203.0.113.11: setting up MMX through verified SSH
   ...
     set_up_nginx / web01 / install_nginx: succeeded, would_change
       [{"name":"nginx-common","to":"1.26.3-3+deb13u10"},{"name":"nginx","to":"1.26.3-3+deb13u10"}]
     set_up_nginx / web01 / shop_site/changes/write_site_file: not_checked, unknown — the parent directory /etc/nginx/conf.d does not exist; create it with an earlier directory node task. The earlier node task install_nginx would change this node first and may provide what is missing; if the run then succeeds, change nothing.
     set_up_nginx / web01 / shop_site/activate/reload_nginx: skipped, unknown — service state can be checked after the node task block's proposed changes are applied
     set_up_nginx / web01 / shop_site/verify/check_shop_site: not_checked, unchanged — Get "http://127.0.0.1:8080/": dial tcp 127.0.0.1:8080: connect: connection refused
   ...
   Run 76f63216: succeeded
   2 finished
   Saved: /home/you/shop/.mmx/runs/20261115T073157.780081110-76f63216ed591b19
   exit code 3
   ```

   **Check:** `install_nginx` says `would_change`, the site file says
   `not_checked`, and the exit code is 3. Exit code 3 means that the
   dryrun worked but could not check some work. The `not_checked` result is expected
   on a fresh server: the nginx package creates `/etc/nginx/conf.d`, and a
   dryrun installs nothing.
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains each result.

8. **Run the playbook.**

   ```sh
   mmx run set_up_web_servers
   ```

   Part of the output, for `web01`:

   ```text
     set_up_nginx / web01 / install_nginx: succeeded, changed
       [{"name":"nginx-common","to":"1.26.3-3+deb13u10"},{"name":"nginx","to":"1.26.3-3+deb13u10"}]
     set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, changed
     set_up_nginx / web01 / shop_site/changes/write_site_file: created "/etc/nginx/conf.d/shop.conf"
       absent -> 85 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, changed — service is not running
     set_up_nginx / web01 / shop_site/verify/check_shop_site: succeeded, unchanged
     set_up_nginx / web01 / shop_site: succeeded, changed
   ...
   Run 5f3e7ce9: succeeded
   ```

   **Check:** the run ends with `Run ...: succeeded` and exit code 0, and
   `curl http://203.0.113.11:8080/` prints `Shop on web01`.

   `reload_nginx` says `service is not running` because `apt_package`
   installs a package without starting its services, so `reload_nginx`
   started nginx. On Debian and Ubuntu, a package's own scripts start a newly
   installed service unless a `policy-rc.d` program forbids it. Ansible's
   `apt` module leaves that alone, so the Ansible task "Start nginx" found
   nginx already running.
   [Packages do not start their services](../reference/modules/apt_package.md#packages-do-not-start-their-services)
   explains the rule.

9. **Run it again.**

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** every node task says `succeeded, unchanged`. If a node task
   says `changed`, a value differs between runs: read the node task's
   line, which says what changed.

10. **Change the variable, and preview on one server.** Step 10 shows how
    MMX does the handler's work. In `inventories/example.yml`, change
    `http_port: 8080` to `http_port: 8081`. Then:

    ```sh
    mmx dryrun set_up_web_servers --limit web01 --diff
    echo "exit code $?"
    ```

    ```text
      set_up_nginx / web01 / shop_site/changes/write_site_file: would update "/etc/nginx/conf.d/shop.conf"
        85 bytes, permissions_mode 0644, uid 0, gid 0 -> 85 bytes, permissions_mode 0644, uid 0, gid 0
        --- before
        +++ after
        @@ -1,5 +1,5 @@
         server {
        -  listen 8080;
        +  listen 8081;
           default_type text/plain;
           return 200 "Shop on web01\n";
         }
      set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, would_change — tracked input /etc/nginx/conf.d/shop.conf changed after the service last started or reloaded
    ```

    **Check:** the diff shows the new port, and `reload_nginx` says
    `would_change`. The check `check_shop_site` says `not_checked`, and
    the exit code is 3, because nothing listens on port 8081 before the
    run.

11. **Run the change.**

    ```sh
    mmx run set_up_web_servers
    ```

    **Check:** on each server, `write_site_file` says `updated` and
    `reload_nginx` says
    `succeeded, changed — tracked input /etc/nginx/conf.d/shop.conf changed after the service last started or reloaded`.
    The run ends with exit code 0, and
    `curl http://203.0.113.12:8081/` prints `Shop on web02`.

The Ansible playbook above, run on a third fresh Debian 13 server, left
the same `/etc/nginx/conf.d/shop.conf`, byte for byte, as the MMX playbook.
nginx was enabled and running in both cases, and listened on the same
ports.

## Commands

| Ansible or Salt | MMX |
| --- | --- |
| `ansible-playbook -i inventories/prod.yml site.yml` | `mmx run site --inventory prod`. Without `--inventory`, MMX uses `default_inventory` from `mmx_project_settings.yml`. [Use one inventory for each environment](choose-which-servers-a-run-touches.md#use-one-inventory-for-each-environment) |
| `ansible-playbook --check --diff` / `salt '*' state.apply test=True` | `mmx dryrun site --diff`. [Preview and run changes](preview-and-run-changes.md) |
| `ansible-playbook --syntax-check` | `mmx validate site`, and `mmx explain site` for every value and the line that each value came from. [Write a playbook](write-a-playbook.md) |
| `--limit web01` | `--limit web01`. [Run on some servers only](choose-which-servers-a-run-touches.md#run-on-some-servers-only) |
| `--list-hosts`, `ansible-inventory --graph` | `mmx nodes` for the inventory's nodes and groups, and `mmx nodes site` for the nodes that each fleet stage selects. [See the selection before you run](choose-which-servers-a-run-touches.md#see-the-selection-before-you-run) |
| `-e release=1.2.3` | `--var release=1.2.3`, for a variable that the playbook declares in `command_line_vars`. [Command-line variables](use-variables-and-templates.md#command-line-variables) |
| `--limit @site.retry` | `mmx resume`, which finishes a saved run's failed and unfinished work and leaves finished nodes alone. [Resume a run](stop-resume-and-read-a-run.md#resume-a-run) |
| `ansible all -m ping` / `salt '*' test.ping` | `mmx run-command --nodes all -- true`. [Run a command on servers now](run-a-command-on-servers-now.md) |
| `ansible web_servers -b --become-user app -m shell -a '...'` / `salt '*' cmd.run '...' runas=app` | `mmx run-command --nodes web_servers --run-as-node-user-account app -- ...`. [Run a command on servers now](run-a-command-on-servers-now.md) |
| `ansible-doc apt` | `mmx describe-module apt_package`. [Find the module for a job](find-the-module-for-a-job.md) |
| `ansible-vault edit` | `mmx secrets edit`. `mmx secrets list-keys`, `diff-keys`, `usage`, `add-random`, and `set` work on the keys without showing a value. [The mmx secrets commands](../reference/secrets.md#the-mmx-secrets-commands) |
| `salt-run jobs.lookup_jid`, returners | `mmx results` reads a run that MMX saved in the project's `.mmx/` directory, and `mmx status` asks the nodes how a run's work stands. [Read a saved run](stop-resume-and-read-a-run.md#read-a-saved-run) |
| `salt-ssh` | MMX itself: `mmx` connects over HTTPS on port 7443, and uses SSH when HTTPS is closed or when the node has `connection: ssh_only`. [A server whose port 7443 is closed](connect-to-your-servers.md#a-server-whose-port-7443-is-closed) |

## Task keywords

| Ansible or Salt | MMX |
| --- | --- |
| `become: true` | Nothing: MMX runs node tasks as root. |
| `become_user: app` | `run_as_user_account: app` on `shell` and on the modules that take the input, such as `git`. [Running as another account](../reference/modules/shell.md#running-as-another-account) |
| `register: result` | The node task's `id`. A later node task of the same fleet stage, on the same node, reads `.node_tasks.<id>.<field>`. A looped node task keeps its results in `.node_tasks.<id>.items`. A fleet stage output carries a value to another fleet stage or node. [Use a result from one server on another](use-a-result-from-one-server-on-another.md) |
| `set_fact` | `set_vars`. The value lasts until the end of the fleet stage, role call, or loop turn that set it. To carry a value across a fleet stage or role boundary, declare an output. [Values computed during the run](use-variables-and-templates.md#values-computed-during-the-run) |
| `debug` | `show`. [Values computed during the run](use-variables-and-templates.md#values-computed-during-the-run) |
| `when`, `loop`, `dict2items` | `when` and `loop` on a node task or a fleet stage. A loop over a mapping gives each turn `.<as>.key` and `.<as>.value`, in the sorted order of the keys, so `dict2items` is not needed. [Decide for each server during the run](choose-which-servers-a-run-touches.md#decide-for-each-server-during-the-run), [Loop over the results](use-a-result-from-one-server-on-another.md#loop-over-the-results) |
| `until`, `retries`, `delay` | A `retry` mapping with `until`, `max_tries`, and `pause_between_tries`. [Node task retries and time limits](../reference/playbook-file.md#node-task-retries-and-time-limits) |
| `handlers` with `register` and `when: result.changed` | A `shell` node task with `only_when_tracked_inputs_changed: true` in a node task block's `activate` phase runs the command only when a tracked input of the block changed, such as a file, a unit, or a package. [Run a command after a change](../reference/modules/shell.md#run-a-command-after-a-change) |
| `changed_when`, `failed_when` | `changed_when` and `failed_when` on `shell`, and `success_exit_codes` for the exit codes that count as success. [Result rules](../reference/modules/shell.md#result-rules) |
| `ignore_errors` | MMX has no counterpart. A module checks the server first, so removing something that is already gone reports `unchanged` instead of failing. For a command whose failure is expected, list its exit codes in `success_exit_codes`. |
| `creates`, `removes` / Salt `creates`, `unless`, `onlyif` | `skip_if_this_path_exists`, `run_only_if_this_path_exists`, `skip_if_this_command_succeeds`, and `run_only_if_this_command_succeeds` on `shell`. [Guards](../reference/modules/shell.md#guards) |
| `chdir`, `environment` | `run_from_directory` and `env` on `shell`. A `shell` script cannot contain a reference to an MMX value, such as `{{ .vars.port }}`, so pass values to the script through `env`. [Where a value comes from](use-variables-and-templates.md#where-a-value-comes-from) |
| `no_log` | Values from the vault are hidden automatically. For a secret that a command prints, set `output_is_secret: true` on `shell`, and for a secret in a file that a node task reads, `content_is_secret: true` on `read_file`. A secret that a module uses only as a login to its tool is hidden without hiding the module's whole result. [Secrets in files, output, and history](../reference/secrets.md#secrets-in-files-output-and-history) |
| `max_fail_percentage`, `any_errors_fatal` | `failed_nodes_allowed`, a count or a percentage of the whole fleet stage and not of each batch, with `on_node_failure: stop_fleet_stage` or `on_node_failure: stop_run`. [Choose what a failed server stops](roll-out-a-change-gradually.md#choose-what-a-failed-server-stops) |
| `hosts: all:!db_servers` | `except_nodes` on the fleet stage. [Leave some servers out of a fleet stage](choose-which-servers-a-run-touches.md#leave-some-servers-out-of-a-fleet-stage) |
| `strategy: free` | The default: with `max_nodes_at_once`, a node starts as soon as another node finishes. `wait_for_all_in_batch: true` finishes each batch before the next one starts. |
| `connection: local`, `local_action`, `delegate_to: localhost` | A Linux controller machine can be a node in the inventory, reached over SSH. MMX has no local mode. A macOS controller machine cannot be a node, and MMX never reboots its controller machine. [Manage the controller machine itself](manage-the-controller-machine-itself.md) |
| A playbook that runs once, such as a migration | A one-time playbook in `one_time_playbooks/`. [Make a one-time change](make-a-one-time-change.md) |
| Salt `require` | Written order: node tasks run in the order of the file, and fleet stages run in the order of the playbook. `requires` on a fleet stage names earlier fleet stages that must have succeeded. |
| YAML anchors and aliases | MMX refuses them: `mmx validate` prints `aliases, anchors, and explicit tags are not supported`. Reuse values with `vars`, and node tasks with roles, which `mmx explain` can trace. |

## Modules

Each MMX module has a page, and `mmx describe-module <name>` prints the
same page. The modules in the last rows are official extra modules, which
come with `mmx` in the same release archive.

| Ansible module | MMX module |
| --- | --- |
| `apt`, `dnf`, `yum`, `package` | [`apt_package`](../reference/modules/apt_package.md) or [`dnf_package`](../reference/modules/dnf_package.md), with a `packages` list. Each entry of an installed package gives `name` and `version`, such as `version: newest_when_first_installed`, and an entry of a package to remove gives only `name`. `dnf_package` drives dnf 4, the dnf of Enterprise Linux 9 and 10, and dnf 5, the dnf of Fedora 41 and newer. There is no `update_cache`: MMX refreshes the package index once for each node job before an install or upgrade, and again after a package source changes. |
| `apt` with `upgrade: dist` | `apt_package` with `upgrade_all_packages_within_their_version_rules: true`, which is apt's `full-upgrade` and leaves held packages back. `version: newest_on_every_run` on named package entries upgrades those packages on every run. [Upgrade the whole node](../reference/modules/apt_package.md#upgrade-the-whole-node) |
| `apt` with `name: nginx=1.26.*`, `dpkg_selections` with `hold` | A `version` pattern on the package entry; the package manager's own rule then keeps the package inside the pattern. [Keep a package at a version](../reference/modules/apt_package.md#keep-a-package-at-a-version) |
| `apt` with `deb:` | `package_file_source`, or `package_file_url` with `package_file_sha256`, on the package entry. The exact `version` of the entry must match the file. [Install a package from a .deb file](../reference/modules/apt_package.md#install-a-package-from-a-deb-file) |
| `dnf` with `security: true` | `dnf_package` with `upgrade_all_packages_within_their_version_rules: true` and `only_security_updates: true`; the node task fails, also in a dryrun, when the node's package sources list no advisories. `apt_package` has no security-only option. On Debian and Ubuntu, see [Apply only security updates on Debian and Ubuntu](../reference/modules/apt_package.md#apply-only-security-updates-on-debian-and-ubuntu). |
| `apt_repository`, `deb822_repository`, `apt_key`, `yum_repository` | [`apt_repository`](../reference/modules/apt_repository.md), [`dnf_repository`](../reference/modules/dnf_repository.md) |
| `pip`, `gem`, `npm`, `snap`, `bundler`, `composer` | `pip_package`, `gem_package`, `npm_global_package`, `snap_package`, and the `..._project_dependencies` modules. [Install languages and application dependencies](install-languages.md) |
| `copy`, `template` | [`file`](../reference/modules/file.md) with `content`, `source`, or `template` |
| `copy` with `remote_src: true`, `assemble` | `file` with `content_from_node_paths`. [Install a file from paths on the node](../reference/modules/file.md#install-a-file-from-paths-on-the-node) |
| `copy` or `template` with `force: false` | `file` with `if_file_exists: keep_existing_content`. [Write a file once, then leave its content alone](../reference/modules/file.md#write-a-file-once-then-leave-its-content-alone) |
| `backup: true` | `file` with `backup_original_file_to`, which keeps the one original file, not a copy for each change. [Keep the original of a file MMX takes over](../reference/modules/file.md#keep-the-original-of-a-file-mmx-takes-over) |
| `file` with `state: directory` or `state: link` | [`directory`](../reference/modules/directory.md), [`symlink`](../reference/modules/symlink.md). `recurse: true` is `also_apply_to_everything_inside: true`. |
| `file` with `state: absent` on a directory | `directory` with `state: absent` and `also_delete_everything_inside: true`. [Delete a directory, or what it no longer lists](../reference/modules/directory.md#delete-a-directory-or-what-it-no-longer-lists) |
| `find` and a loop that deletes | `directory` with `delete_everything_inside_except`, or [`delete_old_files`](../reference/modules/delete_old_files.md) for files older than a duration |
| `synchronize` | `directory` with `source`. `delete: true` is `also_delete_entries_not_in_source: true`. [Copy a directory from the controller machine](../reference/modules/directory.md#copy-a-directory-from-the-controller-machine) |
| `slurp`, `fetch` | [`read_file`](../reference/modules/read_file.md), for a regular file of at most 1 MiB. A fleet stage's `save_on_controller_machine` saves the content on the controller machine. |
| `lineinfile` | [`line`](../reference/modules/line.md). `regexp` is `match`, a Go regular expression, and the replacement text in `line` must match it too. `insertafter` and `insertbefore` are the `insert_...` inputs. When more than one line matches, the node task fails instead of editing the last one. |
| `blockinfile`, `replace`, `ini_file` | A whole file with `file`, or a drop-in file in the program's configuration directory. Otherwise [`edit_file`](../reference/modules/edit_file.md). |
| `get_url` | [`download`](../reference/modules/download.md): MMX fetches an HTTPS file on the controller machine, checks its SHA-256, and sends it to the nodes. `download` accepts no credentials in the URL: for a source that needs credentials, use `git` or `shell`. |
| `unarchive` | [`unpack_archive`](../reference/modules/unpack_archive.md), into a new directory for each release; it never extracts over an existing directory. `unpack_without_wrapping_folder: true` removes one enclosing folder and refuses entries outside it, and is not a general strip-components option. A `symlink` node task then switches `current` to the new release, and the service refresh that the node task block's `refresh` names, reload or restart, follows from the switch. [Unpack a release and switch a symlink](../reference/modules/unpack_archive.md#unpack-a-release-and-switch-a-symlink) |
| `git` | [`git`](../reference/modules/git.md). `version` becomes one of `follow_branch`, `check_out_tag`, or `check_out_commit`, `dest` is `path`, `become_user` is `run_as_user_account`, and `force: true` is `files_changed_in_checkout_on_node: discard_the_changes`. MMX checks out a detached commit as `run_as_user_account`. For an SSH repository, `ssh_private_key` holds the content of the key, normally from a secret, with `ssh_known_hosts_lines`; it does not take a path on the node. The checkout is shallow unless `full_history: true`, and submodules and Git LFS content are not fetched. |
| `user`, `group` | [`user`](../reference/modules/user.md), [`group`](../reference/modules/group.md). `append: false` is `supplementary_groups_not_listed: remove`, and `remove: true` is `also_delete_home_directory_and_mail: true`. `update_password: on_create` is `apply_password_hash: only_when_account_has_no_password`, which also sets a password on an existing account that has none. `user` has no input for account expiry or password aging: run `chage` with `shell` for account expiry and password aging. |
| `authorized_key` | [`authorized_keys`](../reference/modules/authorized_keys.md). `user` is `for_user_account`, and `exclusive: true` is `keys_not_listed: remove`. `key_files_on_controller_machine` and `key_directories_on_controller_machine` read keys from files. |
| `openssh_keypair`, `user` with `generate_ssh_key` | [`ssh_key_pair`](../reference/modules/ssh_key_pair.md). `path`, `type`, `comment`, and `owner` become `private_key_path`, `key_type`, `key_comment_when_created`, and `for_user_account`. `ssh_key_pair` never replaces an existing key, so there is no `force` or `regenerate`. |
| `systemd`, `service` | [`systemd_service`](../reference/modules/systemd_service.md) to start, stop, enable, or mask a unit. [`systemd_unit`](../reference/modules/systemd_unit.md) writes a unit file, and MMX runs `systemctl daemon-reload` after a changed unit. You write `state: running`, `stopped`, or `do_not_start_or_stop`, and `start_at_boot` for boot activation. `scope: user` is `in_systemd_of_user_account`, and `masked: true` masks a unit. |
| `cron` | [`systemd_scheduled_job`](../reference/modules/systemd_scheduled_job.md) (recommended) or [`cron_job`](../reference/modules/cron_job.md) |
| `mount`, `filesystem` | [`fstab_mount`](../reference/modules/fstab_mount.md), [`create_filesystem_on_empty_device`](../reference/modules/create_filesystem_on_empty_device.md); a `.mount` unit also works. |
| `hostname`, `timezone`, `locale_gen` | [`hostname`](../reference/modules/hostname.md), [`time_zone`](../reference/modules/time_zone.md), [`locale`](../reference/modules/locale.md) |
| `sysctl`, `modprobe`, `alternatives` | [`sysctl_setting`](../reference/modules/sysctl_setting.md), [`kernel_module`](../reference/modules/kernel_module.md), [`update_alternatives`](../reference/modules/update_alternatives.md) |
| `ufw`, `firewalld` | [`ufw`](../reference/modules/ufw.md) with `incoming_rules`, `routed_rules`, and `outgoing_rules`; `incoming_rules_not_listed: remove` (and the same for routed and outgoing) makes the node task own that whole set. [`firewalld`](../reference/modules/firewalld.md) manages one zone's services, ports, and rich rules, and `rules_not_listed: remove` owns that set only. [`firewalld_zone`](../reference/modules/firewalld_zone.md) creates or removes zones, and [`firewalld_zone_membership`](../reference/modules/firewalld_zone_membership.md) manages a zone's interfaces and source networks. An interface that NetworkManager controls belongs in `networkmanager_connection_file`. |
| `interfaces_file`, `nmcli`, a template of a network file | One whole network file for the node's network tool: [`ifupdown_interfaces_file`](../reference/modules/ifupdown_interfaces_file.md), [`netplan_file`](../reference/modules/netplan_file.md), [`networkmanager_connection_file`](../reference/modules/networkmanager_connection_file.md), or [`systemd_networkd_file`](../reference/modules/systemd_networkd_file.md). Where `interfaces_file` changes one option, MMX takes the whole file. When a change cuts the node off from the controller machine, the node restores the previous file. `netplan_file` supports only the systemd-networkd renderer; for NetworkManager, use `networkmanager_connection_file`. |
| `seboolean`, `sefcontext`, `seport`, `selinux` | [`selinux_boolean`](../reference/modules/selinux_boolean.md), [`selinux_file_context`](../reference/modules/selinux_file_context.md), [`selinux_port`](../reference/modules/selinux_port.md), [`selinux_mode`](../reference/modules/selinux_mode.md). `selinux_mode` selects `enforcing` or `permissive` and has no disabled state. |
| `acl`, `capabilities` | [`file_access_control_list`](../reference/modules/file_access_control_list.md), [`linux_capability_on_program_file`](../reference/modules/linux_capability_on_program_file.md) |
| `wait_for` | [`wait_for`](../reference/modules/wait_for.md), for a port, a path, an HTTP response, or a command, often in a node task block's `verify` phase |
| `uri` | [`http`](../reference/modules/http.md), with `method`, `json_body`, and `expected_http_status_codes` (default `[200]`). The request is sent once unless the node task sets `retry`. Later node tasks read `.node_tasks.<id>.response_body`, `.response_body_json`, and `.http_status_code`. |
| `validate:` on `copy`, `template`, or `lineinfile` | `check_new_file` on `file`, `download`, `line`, or `edit_file`, such as `visudo -cf` for a sudo rule or `logrotate --debug` for a log rotation file. The check reads the new file at `.new_file.path`, and there are no `sudoers` or `logrotate` modules: write a `file` node task with `check_new_file`. [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md) |
| `assert`, `fail` with `when` | [`assert`](../reference/modules/assert.md), with the condition that must hold |
| `reboot` | [`reboot`](../reference/modules/reboot.md), as the last node task of a fleet stage. `reboot_timeout` is `wait_at_most`, and `test_command` becomes a readiness check in the next fleet stage. |
| `command`, `shell`, `script` | [`shell`](../reference/modules/shell.md), which runs Bash on the node and interprets shell syntax, unlike `command`. For a script file, copy the script with `file`, then run it with `shell`. |
| `docker_compose_v2`, `docker_container`, `docker_image` | [`docker_compose_project`](../reference/modules/docker_compose_project.md) and [`docker_image`](../reference/modules/docker_image.md). For a single container, use a Compose file with one service, or a systemd unit: [Run containers](run-containers.md). |
| `podman_container`, `podman_image` | [`podman_quadlet`](../reference/modules/podman_quadlet.md), [`podman_image`](../reference/modules/podman_image.md) |
| `postgresql_user`, `postgresql_db`, `mysql_user`, `mysql_db` | [`postgresql_role`](../reference/modules/postgresql_role.md), [`postgresql_database`](../reference/modules/postgresql_database.md), [`mysql_user`](../reference/modules/mysql_user.md), [`mysql_database`](../reference/modules/mysql_database.md). [Manage a database](manage-a-database.md) |
| `acme_certificate`, a certbot command | [`certbot_certificate`](../reference/modules/certbot_certificate.md) |
| `parted`, or a `growpart` command | [`create_partitions_on_empty_disk`](../../official_extra_modules/create_partitions_on_empty_disk/README.md) writes a GPT partition table with named partitions on a disk that holds nothing, and never changes a disk that holds anything. [`grow_partition_into_free_space_after_it`](../../official_extra_modules/grow_partition_into_free_space_after_it/README.md) grows one existing partition into free space after it and leaves its contents alone. |
| `filesystem` with `resizefs: true` | [`grow_filesystem_to_fill_device`](../../official_extra_modules/grow_filesystem_to_fill_device/README.md), for an ext4 or XFS filesystem mounted for writing. It grows the filesystem and never shrinks it. |
| `lvg`, `lvol` | [`lvm_volume_group`](../../official_extra_modules/lvm_volume_group/README.md), [`lvm_logical_volume`](../../official_extra_modules/lvm_logical_volume/README.md). `pvs` is `physical_volumes`, `vg` is `volume_group`, and `resizefs` is `grow_filesystem_with_volume`, which you set explicitly: `true` grows the filesystem with the volume, and `false` grows only the volume. There is no `shrink` or `force`. |
| `zfs` | [`zfs_dataset`](../../official_extra_modules/zfs_dataset/README.md). `extra_zfs_properties` is `properties`, and `state: absent` is `state: deleted_with_all_its_data`. |
| `lxd_container`, `lxd_profile` | [`incus_instance`](../../official_extra_modules/incus_instance/README.md), [`incus_profile`](../../official_extra_modules/incus_profile/README.md), for Incus |
| `virt` | [`libvirt_domain`](../../official_extra_modules/libvirt_domain/README.md). The XML comes from a file on the node that a `file` node task writes, and `autostart` is `start_at_boot`. |
| `rabbitmq_vhost`, `rabbitmq_user`, `rabbitmq_policy`, `rabbitmq_plugin` | [`rabbitmq_virtual_host`](../../official_extra_modules/rabbitmq_virtual_host/README.md), [`rabbitmq_user`](../../official_extra_modules/rabbitmq_user/README.md), [`rabbitmq_policy`](../../official_extra_modules/rabbitmq_policy/README.md), [`rabbitmq_plugin`](../../official_extra_modules/rabbitmq_plugin/README.md). Each `rabbitmq_user.permissions` entry gives `virtual_host`, `configure`, `write`, and `read` instead of `vhost`, `configure_priv`, `write_priv`, and `read_priv`, and `permissions_not_listed: remove` or `keep` is required. |
| `mongodb_user` | [`mongodb_user`](../../official_extra_modules/mongodb_user/README.md). `update_password` has no counterpart: the module sets the password only when the user cannot log in with it. |

Salt's common state functions map to the same modules: `pkg.installed`
to `apt_package` or `dnf_package`, `pkgrepo.managed` to `apt_repository`
or `dnf_repository`, `file.managed` to `file`, `service.running` to
`systemd_service`, and `cmd.run` to `shell`. A `salt://` path becomes a
path in your project, such as `files/shop.conf.tmpl`, or in a role's
directory inside a role.

When no module does the job, [When no module fits](find-the-module-for-a-job.md#when-no-module-fits)
lists the smallest tool for it.

## Not built, and why

These operations have no MMX module or input. Each row gives the reason
and the way to do the work.

| Ansible or Salt work | In MMX |
| --- | --- |
| NAT masquerading and port forwarding in `ufw` or `firewalld` | `ufw` has no NAT command, and Incus, libvirt, Docker, and Podman write their own NAT rules. Write the NAT rules as a validated file with an explicit activation node task; [`ufw`](../reference/modules/ufw.md) shows a gateway with a `nft` rules file. |
| Security-only upgrades with `apt` | apt has no security-only selection. Debian and Ubuntu use `unattended-upgrade`; the [security updates example](../../examples/security-updates-on-debian-and-ubuntu/README.md) configures it and applies updates on demand. |
| Snap refresh schedules and holds | Run the `snap` command in a guarded `shell` node task. `snap_package` installs snaps. |
| Account expiry and password aging | Run `chage` with `shell`. |
| A condition that picks one entry of a list fact, such as one mount of `.node.storage.mounts` | Conditions cannot pick one mount or block device. To check that `/var` has 10 GiB free, run `df` in a `shell` node task and test its output with a `number_at_least` condition. |
| Cloud inventory generators inside MMX | Each provider needs its own API integration, credentials, and tests against the real provider. Any program can write the inventory YAML that MMX reads: [Inventory file](../reference/inventory-file.md). |
| A module for sudo rules, log rotation, time synchronization, or NFS exports | Combine a package node task, a `file` node task with `check_new_file`, and a service node task. A module is added only when it does more than wrap a configuration file. |

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx validate` prints `line 3: aliases, anchors, and explicit tags are not supported` | The file uses a YAML anchor or alias, such as `&web` and `*web`. Write the value out, or move it to `vars`. |
| `mmx explain` says `reference .vars.http_port is not declared for node web01` | The node does not get the variable. Check that the variable is in the `vars` of the node's group, the node, the inventory, or the project. [Use variables and templates](use-variables-and-templates.md#if-something-is-different) lists the causes. |
| The node belongs to two groups that set the same variable | MMX refuses the inventory. Set the variable in one group only, in the inventory's `vars`, or in each node's `vars`. |
| A converted Jinja2 template fails with an unknown function | Go templates have other functions than Jinja2 filters. [Functions in templates](use-variables-and-templates.md#functions-in-templates) lists the functions. |
| The server runs Fedora, Rocky Linux, or AlmaLinux | Use `dnf_package` instead of `apt_package` in step 4, with the same inputs. |
| You used Ansible's `async` with `poll: 0` | MMX has no counterpart. The node keeps working on a fleet stage if the controller machine disconnects, but node tasks still run in order. Run a long-lived program as a systemd service with `systemd_unit` and `systemd_service`. |
| A package installed on Debian or Ubuntu did not start its service | `apt_package` stops package scripts from starting services. Set `allow_package_to_start_its_services: true` on the node task when the installation needs the service. Package scripts may still restart a service that already runs. [Packages do not start their services](../reference/modules/apt_package.md#packages-do-not-start-their-services) |
| Ansible's `append`, `exclusive`, or `purge` had other defaults for each module | A node task that owns a list has one required input, `<things>_not_listed`, with the value `remove` or `keep`, for example `keys_not_listed` or `rules_not_listed`. |
| You used `pause` or `vars_prompt` | MMX has no prompt in a playbook, because a coding agent or CI cannot answer a prompt. Run `mmx dryrun` before `mmx run`, and pass a value with `--var`. |

## Where to go next

- [Write a playbook](write-a-playbook.md): a playbook from the start,
  without Ansible.
- [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
  checking a new file before nginx loads it, and putting the old file back
  when a check fails.
- [Use variables and templates](use-variables-and-templates.md): every
  place a value can come from, and the template functions.
- [Reuse work with roles](reuse-work-with-roles.md): turning an Ansible
  role into an MMX role.
- [Roll out a change gradually](roll-out-a-change-gradually.md): `serial`
  and `max_fail_percentage` in MMX.
- [Coming from Ansible](../../examples/subscription-web-service-fleet/README.md#coming-from-ansible):
  the Ansible habits that the subscription web service example replaced.
  The example comes from a real company's Ansible repository, with invented
  names and services and without product-specific parts.
