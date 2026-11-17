# Use a result from one server on another

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** a value that one server holds or produces is
needed on other servers. For example, the app servers' configuration needs
the database server's address and a key that the database server generated,
and the load balancer's configuration needs the list of app servers.

**What this page does:** the database server `db01` publishes its address
and a generated key. The app servers `web01` and `web02` write both values
into their configuration file and publish their own addresses. The load
balancer `lb01` builds its list of app servers from those addresses. Later
sections cover command output, loops over results, work for each server in
turn, and saving values to files on your controller machine.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`, where **groups** name lists of nodes.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**, run
  in order. Each fleet stage names nodes with `nodes`, and each of those
  nodes runs the fleet stage's **node tasks**.
- A node task's **result** holds what the node task found or did, such as
  the `content` of a file that a `read_file` node task read. Later node
  tasks of the same fleet stage, on the same node, read the result as
  `.node_tasks.<node task id>.<field>`.
- A **fleet stage output** is a value that a fleet stage publishes from
  each of its nodes, under `outputs`. Later fleet stages read the output on
  any node as `.outputs.<fleet stage id>.<node>.<output name>`. A node task
  result never reaches another fleet stage or another node by itself: an
  output carries the value.

| The value you need | Go to |
| --- | --- |
| Exists only on a server, such as a key that `db01` generated | [Steps](#steps) |
| Comes from each of several servers, such as the app servers' addresses for the load balancer | [Steps](#steps), steps 3 and 4 |
| Is in your inventory already, such as the address MMX uses to reach `db01` | [When the value is in your inventory](#when-the-value-is-in-your-inventory) |
| Is printed by a command, such as JSON | [Read a value from a command's output](#read-a-value-from-a-commands-output) |
| Is needed for each server in turn, with work on another server, such as a key for each app server generated on `db01` | [Do one server's work on another server](#do-one-servers-work-on-another-server) |
| Should end up in a file on your controller machine | [Save outputs to files on your controller machine](#save-outputs-to-files-on-your-controller-machine) |

## Before you start

- You have a project where `mmx run` worked, as in
  [Preview and run changes](preview-and-run-changes.md).
- The web servers `web01` and `web02`, in the group `web_servers`, serve
  the home page `Shop` with nginx on port 8080, as the playbook
  `set_up_web_servers` from [Write a playbook](write-a-playbook.md) sets
  up the web servers.
- Two more servers run Debian or Ubuntu: `db01` at `203.0.113.21`, the
  database server, and `lb01` at `203.0.113.31`, the load balancer. You
  can log in to `db01` and `lb01` with `ssh` as `root`, and your
  controller machine trusts the two servers' SSH host keys. A coding
  agent, which cannot answer the host key question, first compares each
  fingerprint from
  `ssh-keyscan -t ed25519 203.0.113.21 | ssh-keygen -lf -` with the
  fingerprint that the hosting provider shows, and then logs in with
  `ssh -o StrictHostKeyChecking=accept-new root@203.0.113.21 true`.

## Steps

1. **Add the database server and the load balancer to the inventory.**

   ```yaml
   # inventories/example.yml
   nodes:
     web01: {address: 203.0.113.11, ssh_user: root}
     web02: {address: 203.0.113.12, ssh_user: root}
     db01: {address: 203.0.113.21, ssh_user: root}
     lb01: {address: 203.0.113.31, ssh_user: root}

   groups:
     web_servers:
       nodes: [web01, web02]
   ```

   **Check:** `mmx nodes` lists four nodes:

   ```text
   Nodes
     db01    203.0.113.21   root
     lb01    203.0.113.31   root
     web01   203.0.113.11   root   web_servers
     web02   203.0.113.12   root   web_servers
   ```

2. **Publish two values from the database server.** Create
   `playbooks/connect_servers.yml` with a first fleet stage that runs on
   `db01`:

   ```yaml
   fleet_stages:
     - id: prepare_database
       nodes: db01
       node_tasks:
         - id: create_config_directory
           directory:
             path: /etc/shop
             permissions_mode: "0700"

         - id: generate_app_key
           shell:
             script: umask 077; head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/shop/app_key
             skip_if_this_path_exists: /etc/shop/app_key   # create the key only once

         - id: read_app_key
           read_file:
             path: /etc/shop/app_key
             content_is_secret: true   # the key is a secret: MMX never prints the key
       outputs:                        # values that later fleet stages read
         address: "{{ .node.network.default_ipv4_address }}"
         app_key: "{{ .node_tasks.read_app_key.content }}"
   ```

   The `shell` command stands for the program that creates a key on your
   database server, such as the password of the app's database account.
   `read_file` reads the key file on `db01`, and its result field `content`
   holds the file's text. `outputs` then publishes two values from `db01`:

   - `address` is the fact `.node.network.default_ipv4_address`: the IPv4
     address that `db01` sends its default route's packets from.
     [Node facts](use-variables-and-templates.md#node-facts) says when MMX
     gathers facts.
   - `app_key` is the content of the key file. Because `read_app_key` says
     `content_is_secret: true`, the output is a secret too: MMX never prints
     the key, and the run's saved history holds no copy of the key.

   **Check:** `mmx validate connect_servers` prints `Valid: mmx_example`.

3. **Use the database server's values on the app servers.** Add a second
   fleet stage below the first one, at the same indent:

   ```yaml
     - id: configure_app
       nodes: web_servers
       node_tasks:
         - id: create_config_directory
           directory:
             path: /etc/shop

         - id: write_app_config
           file:
             path: /etc/shop/app.conf
             permissions_mode: "0600"   # the file holds a secret, so only root may read the file
             content: |
               database_address = {{ .outputs.prepare_database.db01.address }}
               database_key = {{ .outputs.prepare_database.db01.app_key }}
       outputs:
         address: "{{ .node.network.default_ipv4_address }}"
   ```

   `.outputs.prepare_database.db01.address` reads the output `address` that
   the fleet stage `prepare_database` published on the node `db01`. The
   `outputs` of `configure_app` publish each app server's own address, for
   the load balancer in step 4.

   A file whose content holds a secret needs a mode that only the owner can
   read, such as `"0600"`. [Files that hold secrets](../reference/modules/file.md#files-that-hold-secrets)
   gives the rule.

   **Check:** `mmx validate connect_servers` prints `Valid: mmx_example`.

4. **Build the load balancer's list of app servers.** Create the template
   `files/load_balancer.conf.tmpl`:

   ```text
   upstream shop_app_servers {
   {{- range $node, $outputs := .outputs.configure_app }}
     server {{ $outputs.address }}:8080;  # {{ $node }}
   {{- end }}
   }

   server {
     listen 80;
     location / {
       proxy_pass http://shop_app_servers;
     }
   }
   ```

   `.outputs.configure_app`, without a node name, is the fleet stage's
   **whole output collection**: a mapping from each node's name to that
   node's outputs. `range` writes one `server` line for each app server, in
   the order of the node names. With a third app server in `web_servers`,
   the next run writes three lines.

   Then add the third fleet stage below the second one:

   ```yaml
     - id: configure_load_balancer
       nodes: lb01
       node_tasks:
         - id: install_nginx
           apt_package:
             packages:
               - {name: nginx, version: newest_when_first_installed}

         - id: check_each_app_server
           loop: "{{ .outputs.configure_app }}"   # one iteration for each app server
           as: app_server
           wait_for:
             url_responds: "http://{{ .app_server.value.address }}:8080/"
             response_body_contains: Shop

         - task_block:
             id: load_balancer_site
             changes:
               - id: remove_default_site
                 symlink:
                   path: /etc/nginx/sites-enabled/default
                   state: absent
               - id: write_load_balancer_site
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   template: files/load_balancer.conf.tmpl
             activate:
               - id: reload_nginx
                 systemd_service:
                   name: nginx
                   start_at_boot: true
                   state: running
                   refresh: reload
             verify:
               - id: check_shop_through_load_balancer
                 wait_for:
                   url_responds: http://127.0.0.1/
                   response_body_contains: Shop
   ```

   `check_each_app_server` loops over the same collection: for each app
   server, `.app_server.key` is the node's name and `.app_server.value`
   holds the app server's outputs. From `lb01`, the loop checks that each
   app server answers before the load balancer sends traffic to the app
   servers. The nginx package of Debian and Ubuntu enables a default site
   on port 80, so `remove_default_site` removes the default site's link. The node task block writes the site,
   starts nginx if nginx is stopped, reloads nginx when a file in `changes`
   differs from what nginx last loaded, and checks the shop through the
   load balancer.
   [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md)
   explains node task blocks.

   **Check:** `mmx validate connect_servers` prints `Valid: mmx_example`,
   and `mmx nodes connect_servers` shows each fleet stage's nodes:

   ```text
   Fleet stages
     prepare_database          db01          → db01
     configure_app             web_servers   → web01, web02
     configure_load_balancer   lb01          → lb01
   ```

   The whole of `playbooks/connect_servers.yml` now reads:

   ```yaml
   fleet_stages:
     - id: prepare_database
       nodes: db01
       node_tasks:
         - id: create_config_directory
           directory:
             path: /etc/shop
             permissions_mode: "0700"

         - id: generate_app_key
           shell:
             script: umask 077; head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/shop/app_key
             skip_if_this_path_exists: /etc/shop/app_key

         - id: read_app_key
           read_file:
             path: /etc/shop/app_key
             content_is_secret: true
       outputs:
         address: "{{ .node.network.default_ipv4_address }}"
         app_key: "{{ .node_tasks.read_app_key.content }}"

     - id: configure_app
       nodes: web_servers
       node_tasks:
         - id: create_config_directory
           directory:
             path: /etc/shop

         - id: write_app_config
           file:
             path: /etc/shop/app.conf
             permissions_mode: "0600"
             content: |
               database_address = {{ .outputs.prepare_database.db01.address }}
               database_key = {{ .outputs.prepare_database.db01.app_key }}
       outputs:
         address: "{{ .node.network.default_ipv4_address }}"

     - id: configure_load_balancer
       nodes: lb01
       node_tasks:
         - id: install_nginx
           apt_package:
             packages:
               - {name: nginx, version: newest_when_first_installed}

         - id: check_each_app_server
           loop: "{{ .outputs.configure_app }}"
           as: app_server
           wait_for:
             url_responds: "http://{{ .app_server.value.address }}:8080/"
             response_body_contains: Shop

         - task_block:
             id: load_balancer_site
             changes:
               - id: remove_default_site
                 symlink:
                   path: /etc/nginx/sites-enabled/default
                   state: absent
               - id: write_load_balancer_site
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   template: files/load_balancer.conf.tmpl
             activate:
               - id: reload_nginx
                 systemd_service:
                   name: nginx
                   start_at_boot: true
                   state: running
                   refresh: reload
             verify:
               - id: check_shop_through_load_balancer
                 wait_for:
                   url_responds: http://127.0.0.1/
                   response_body_contains: Shop
   ```

5. **Preview the playbook.**

   ```sh
   mmx dryrun connect_servers
   echo "exit code $?"
   ```

   The first dryrun on `db01` and `lb01` also sets up MMX on the two
   servers. Before the first run, the key file on `db01` doesn't exist yet,
   so the dryrun can't know the key. Every value that depends on the key
   waits for the run:

   ```text
   prepare_database / db01: not_checked
     playbooks/connect_servers.yml:21:16 (fleet_stages[0].outputs.app_key): .node_tasks.read_app_key.content: read_app_key is not_checked: file does not exist: /etc/shop/app_key. The earlier node task create_config_directory would change this node first and may provide what is missing; if the run then succeeds, change nothing.; value supplied at playbooks/connect_servers.yml:15:9 (fleet_stages[0].node_tasks[2])
   configure_app / web01: succeeded
   configure_app / web02: succeeded
   configure_load_balancer / lb01: succeeded
   ...
     prepare_database / db01 / generate_app_key: skipped, unknown — the shell node task has no dryrun command; set dryrun to a read-only command, or run_script_in_dryrun: true for a script that changes nothing, to check the task
     prepare_database / db01 / read_app_key: not_checked, unknown — file does not exist: /etc/shop/app_key. The earlier node task create_config_directory would change this node first and may provide what is missing; if the run then succeeds, change nothing.
   ...
     configure_app / web01 / write_app_config: not_checked, unknown — playbooks/connect_servers.yml:34:20 (fleet_stages[1].node_tasks[1].file.content): .outputs.prepare_database.db01.address: prepare_database on db01 is not_checked: ...
   ...
     configure_load_balancer / lb01 / check_each_app_server[web01]: succeeded, unchanged
     configure_load_balancer / lb01 / check_each_app_server[web02]: succeeded, unchanged
   ...
   Run 3f766dc5: succeeded
   4 finished
   exit code 3
   ```

   **Check:** the last lines say `Run ...: succeeded`, and the exit code is
   3: some work could not be checked. On a first dryrun, these results are
   expected:

   - `generate_app_key` is skipped, because a dryrun doesn't run a `shell`
     node task that has no `dryrun` command.
   - `read_app_key` is `not_checked`, because the key file doesn't exist
     yet. The message names the earlier node task that would change `db01`
     first.
   - `prepare_database` on `db01` is `not_checked`, because the output
     `app_key` can't be computed. A reader of any output of
     `prepare_database` on `db01`, `address` too, then waits:
     `write_app_config` is `not_checked` and names the chain of causes.
   - The app servers' `address` outputs are known in a dryrun, because a
     dryrun gathers facts too. So `check_each_app_server` already checks
     both app servers from `lb01`.

   Change nothing in the playbook for these lines. A later dryrun, after
   the first run, knows the key and shows what would change.
   [What a dryrun shows before a value exists](#what-a-dryrun-shows-before-a-value-exists)
   says more.

6. **Run the playbook.**

   ```sh
   mmx run connect_servers
   ```

   **Check:** the run ends with `Run ...: succeeded` and exit code 0. On the
   first run, `generate_app_key` says `changed`, and each file says
   `created`:

   ```text
   prepare_database / db01: succeeded
   configure_app / web01: succeeded
   configure_app / web02: succeeded
   configure_load_balancer / lb01: succeeded
   ...
     prepare_database / db01 / generate_app_key: succeeded, changed
     prepare_database / db01 / read_app_key: succeeded, unchanged
   ...
     configure_app / web01 / write_app_config: succeeded, changed
     configure_app / web01 / write_app_config: created "/etc/shop/app.conf"
       absent -> 80 bytes, permissions_mode 0600, uid 0, gid 0
   ...
     configure_load_balancer / lb01 / load_balancer_site/changes/write_load_balancer_site: created "/etc/nginx/conf.d/shop.conf"
       absent -> 188 bytes, permissions_mode 0644, uid 0, gid 0
     configure_load_balancer / lb01 / load_balancer_site/activate/reload_nginx: succeeded, changed — service is not running
     configure_load_balancer / lb01 / load_balancer_site/verify/check_shop_through_load_balancer: succeeded, unchanged
   ...
   Run c07180e8: succeeded
   ```

   `reload_nginx` says `service is not running` on the first run, because
   MMX installs a package without starting its services, so MMX started
   nginx instead of reloading it. [Reload or restart](change-a-configuration-and-reload-its-service.md#reload-or-restart)
   says more.

   See the outputs that a fleet stage published with `mmx results` and the
   run's ID:

   ```sh
   mmx results c07180e8 --fleet-stage prepare_database
   ```

   ```text
   prepare_database / db01: succeeded
     outputs: {"address":"203.0.113.21","app_key":"[redacted]"}
   ```

   The key shows as `[redacted]`, because the key is a secret. Then ask the
   load balancer for the shop from your controller machine:

   ```sh
   curl http://203.0.113.31/
   ```

   The command prints `<h1>Shop</h1>`. On `lb01`,
   `/etc/nginx/conf.d/shop.conf` lists both app servers:

   ```text
   upstream shop_app_servers {
     server 203.0.113.11:8080;  # web01
     server 203.0.113.12:8080;  # web02
   }
   ```

7. **Run the playbook again.**

   ```sh
   mmx run connect_servers
   ```

   **Check:** every node task says `unchanged`, and `generate_app_key`
   says `skipped, unchanged — node task skipped by a path condition for:
   /etc/shop/app_key`, because the key file exists. The key stays the same
   from run to run, so the app servers' files stay the same too.

## What a dryrun shows before a value exists

A dryrun changes nothing, so a value that only the run creates is unknown
in a dryrun. MMX then reports the work that needs the value as
`not_checked`, not as false or empty, and names the cause:

- A `read_file` node task whose file doesn't exist yet is `not_checked`.
  A run then reads the file that an earlier node task created.
- A fleet stage whose output reads an unknown value is `not_checked` on
  that node. Every reader of that fleet stage's outputs on that node is
  `not_checked` too, also on other nodes and in later fleet stages, and
  the dryrun exits with code 3.
- A fact, a variable, or an inventory value is known in a dryrun too,
  because a dryrun gathers facts as a run does
  ([Node facts](use-variables-and-templates.md#node-facts)). The dryrun
  then checks the work that reads the value, as `check_each_app_server`
  shows in step 5.
- When the values are known, a dryrun shows the change. For a file that
  holds a secret, the dryrun shows the sizes but not the text: `contents
  suppressed for a task using secrets`.

`mmx explain connect_servers` shows a value that comes from an output as
known only during the run:

```text
    file.content = "runtime (not evaluated)"
      from playbooks/connect_servers.yml:34:20 (fleet_stages[1].node_tasks[1].file.content)
      reference "database_address = {{ .outputs.prepare_database.db01.address }}\ndatabase_key = {{ .outputs.prepare_database.db01.app_key }}\n"
```

[Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
explains `not_checked` and the exit codes.

## How a later fleet stage reads an output

| Reference | What the reference reads |
| --- | --- |
| `.node_tasks.<id>.<field>` | A result field of an earlier node task in the same fleet stage, on the same node, such as `.node_tasks.read_app_key.content`. |
| `.node_tasks.<id>.items` | A looped node task's results, one for each iteration, in order, each with its `iteration_key`. |
| `.node_tasks.<id>.outputs.<name>` | An output that a role declares. [Roles](reuse-work-with-roles.md) explains roles. |
| `.outputs.<fleet stage>.<node>.<name>` | One output that an earlier fleet stage published on one node. |
| `.outputs.<fleet stage>` | The whole output collection: each node's name with that node's outputs. |
| `.outputs.<fleet stage>[.<variable>].<name>` | One node's output, with the node's name taken from a variable, such as a loop's `as` variable. |
| `.fleet_stages.<fleet stage>.<node>.status` | How an earlier fleet stage ended on one node, also after a failure. |

The rules that decide what a reader gets:

- **The fleet stage's node tasks must succeed first.** MMX computes a
  node's outputs after the fleet stage's node tasks have succeeded on that
  node. Reading one node's output waits until that fleet stage has succeeded
  on that node. The reading fleet stage needs no `requires` setting for
  this.
- **A failed producer blocks the reader.** When `prepare_database` failed
  on `db01`, the node task that reads the fleet stage's output is
  `blocked`, with the cause, and the run fails. The node tasks before the blocked node task
  still run. A successful node's output stays readable when another node
  of the same fleet stage fails.
- **A whole output collection needs every node.** Reading
  `.outputs.configure_app` needs the outputs of every node that the fleet
  stage selected, so a failed node never drops out of the list unnoticed.
  A node that the fleet stage's `when` skipped is left out of the
  collection. Reading the skipped node's own entry is blocked, and the
  message names the condition.
- **Only earlier fleet stages.** A reference to a fleet stage later in the
  playbook, or to an output name that the fleet stage doesn't declare, is
  refused before any connection.
- **An output keeps its type.** An output can be text, a number, a
  boolean, a list, or a mapping, such as `update_needed: {equals:
  ["{{ .node_tasks.check.exit_code }}", 2]}`, which gives a boolean. A
  whole-value reference, such as `loop: "{{ .outputs.configure_app }}"`,
  keeps the type. A list or a mapping inside other text is an error. A
  template formats a list or a mapping instead.
- **A fleet stage may publish without node tasks.** A fleet stage with
  `outputs` and no `node_tasks` publishes facts or values only, such as
  each app server's `address`.
- **A limit must keep the producers.** MMX refuses a `--limit` that
  leaves out the nodes that publish an output while a kept fleet stage
  reads the output. The refusal comes before any connection, and
  [Choose which servers a run touches](choose-which-servers-a-run-touches.md#if-something-is-different)
  lists the refusals of `--limit`; the table at the end of this page has
  the message. `mmx dryrun connect_servers --limit db01,web01` keeps
  `db01` and `web01`, and leaves out `configure_load_balancer`.
- **`mmx resume` keeps completed outputs.** A resumed run doesn't repeat a
  fleet stage that already succeeded: the later fleet stages get the
  outputs that the earlier attempt saved. A saved output is what the node
  reported then. A new `mmx run` computes every output again.
- **A secret output is not saved in the run's records.** For an output
  made from a `read_file` node task with `content_is_secret: true`, the
  saved run keeps only where the value came from: the fleet stage, the
  node, the node task, and the file's path. `mmx resume` handles a secret
  output in its own way, which
  [Secrets that start on a node](../reference/secrets.md#secrets-that-start-on-a-node)
  explains in full.

## When the value is in your inventory

The inventory is known before the run, so a node task can read an
inventory value directly, with no fleet stage output. For example, the
address that MMX uses to reach `db01` is
`.inventory.nodes.db01.address`:

```yaml
# playbooks/point_app_at_database.yml
fleet_stages:
  - id: configure_app
    nodes: web_servers
    node_tasks:
      - id: create_config_directory
        directory:
          path: /etc/shop

      - id: write_database_address
        file:
          path: /etc/shop/database.conf
          permissions_mode: "0644"
          content: |
            database_address = {{ .inventory.nodes.db01.address }}
```

`mmx explain point_app_at_database` shows the value for each app server:
`file.content = "database_address = 203.0.113.21\n"`. A dryrun shows the
difference, also with `--limit web01`, because no other node is needed:

```text
  configure_app / web01 / write_database_address: would create "/etc/shop/database.conf"
    absent -> 33 bytes, permissions_mode 0644, uid 0, gid 0
    --- before
    +++ after
    @@ -0,0 +1,1 @@
    +database_address = 203.0.113.21
```

[Values about other servers](use-variables-and-templates.md#values-about-other-servers)
lists what `.inventory` holds, such as `.inventory.groups.web_servers`,
the list of the group's node names.

Use an output instead when the value is not in the inventory:

- The inventory's `address` is the address that your controller machine
  connects to. The inventory's `address` can be a public address, a host
  name, or a `Host` name from your `~/.ssh/config`. The app servers can't
  use a `Host` name, and they may need another address, such as a private
  one. A fact such as `.node.network.default_ipv4_address` is the address
  as the server itself sees it.
- A value that a server generates, such as a key, exists only on that
  server.

## Read a value from a command's output

A `shell` node task's result holds `stdout`, the command's output as text,
and `stdout_json`, the same output read as JSON. Read a field of the JSON
by its name, and an item of a list by its position in brackets, from 0.

For example, a server at a hosting provider often has a public address on
its default route and a private address on a second network interface.
`ip -json -4 address show dev eth0` prints the addresses of the interface
`eth0` as JSON. In the fleet stage `prepare_database`, add this node task
after `read_app_key`, and change the output `address`:

```yaml
      - id: private_address
        shell:
          script: ip -json -4 address show dev eth0
          run_script_in_dryrun: true   # read-only, so a dryrun runs the script too
          changed_when: false          # reading changes nothing
    outputs:
      address: "{{ .node_tasks.private_address.stdout_json[0].addr_info[0].local }}"
      app_key: "{{ .node_tasks.read_app_key.content }}"
```

Here `eth0` holds the default-route address, so the command prints the
same address that `.node.network.default_ipv4_address` gives. On a server
with a private network, put the name of your private network's interface
in place of `eth0` to read the private address. The command prints a
list with one entry for the interface:

```text
[{"ifindex":4280,...,"ifname":"eth0",...,"addr_info":[{"family":"inet","local":"203.0.113.21","prefixlen":16,...}]}]
```

`stdout_json[0].addr_info[0].local` is the first address of the first
entry. A dryrun then shows `private_address: succeeded, unchanged`, and
`mmx results --fleet-stage prepare_database` shows the same output as
before: `outputs: {"address":"203.0.113.21","app_key":"[redacted]"}`.

- Reading `stdout_json` of output that isn't valid JSON is an error that
  says so. A missing field is an error too, never an empty value.
- `run_script_in_dryrun: true` and `changed_when: false` matter for a command
  that only reads: without `run_script_in_dryrun` (or a `dryrun` command), a
  dryrun skips the command, and every value read from the command's output
  waits for the run. `run_script_in_dryrun: true` says the script changes
  nothing; MMX does not check that.
- **A resume keeps outputs.** `mmx resume` keeps the outputs of completed
  work and the node and loop selections that the run resolved. A new
  attempt of an unfinished fleet stage reads the current results of the
  earlier fleet stages of the run that `mmx resume` picks. The attempt
  still uses the run's saved playbook, templates, and inventory.
- The `http` and `wait_for` modules give `response_body_json` in the same
  way. `mmx describe-module NAME --reference-only` lists each module's result
  fields on its `Result:` line.

## Loop over the results

`check_each_app_server` in step 4 is a node task with `loop` and `as`. A
loop repeats one node task, role call, or node task block for each item:

- A **list** runs in the written order, and the `as` variable holds each
  item.
- A **mapping**, such as a whole output collection, runs in the sorted
  order of its keys. The `as` variable holds `key` and `value`, such as
  `.app_server.key` (`web01`) and `.app_server.value.address`.

Each iteration has its own line in the output, named after the key:

```text
  configure_load_balancer / lb01 / check_each_app_server[web01]: succeeded, unchanged
  configure_load_balancer / lb01 / check_each_app_server[web02]: succeeded, unchanged
  configure_load_balancer / lb01 / check_each_app_server: succeeded, unchanged
```

A later node task in the same fleet stage reads the iterations' results in
`.node_tasks.check_each_app_server.items`: a list with one result for each
iteration, in order, each with its `iteration_key`, such as `web01`.

A looped node task block, and a loop over a `shell` node task with
`only_when_tracked_inputs_changed`, also needs a `key` that names each
iteration in a stable way, such as `key: "{{ .app_server.key }}"`. The
names `node_tasks` and `this_node_task` can't be an `as` name.

## Do one server's work on another server

A `for_each_node` loop runs a list of fleet stages for each node of a
group, one node after the other. Use the loop when each app server needs
work on another server, such as its own key generated on `db01`:

```yaml
# playbooks/give_each_app_server_a_key.yml
fleet_stages:
  - id: app_server_keys
    for_each_node: web_servers   # one iteration for each app server
    as: web_server               # the current app server's name
    fleet_stages:
      - id: create_key_on_database
        nodes: db01              # runs on db01, for the current app server
        node_tasks:
          - id: create_keys_directory
            directory:
              path: /etc/shop/keys
              permissions_mode: "0700"

          - id: generate_key
            shell:
              script: umask 077; head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > "/etc/shop/keys/$SERVER"
              env: {SERVER: "{{ .web_server }}"}
              skip_if_this_path_exists: "/etc/shop/keys/{{ .web_server }}"

          - id: read_key
            read_file:
              path: "/etc/shop/keys/{{ .web_server }}"
              content_is_secret: true
        outputs:
          key: "{{ .node_tasks.read_key.content }}"

      - id: write_key_on_app_server
        nodes: "{{ .web_server }}"   # runs on the current app server
        node_tasks:
          - id: write_key_file
            file:
              path: /etc/shop/own_key
              permissions_mode: "0600"
              content: "{{ .outputs.create_key_on_database.db01.key }}"
```

Inside an iteration, `.outputs.create_key_on_database.db01.key` reads the
output of the same iteration, so `web01` and `web02` each get the key that
`db01` generated for that app server. A `shell` script takes no
references, so the script gets the app server's name through `env`.

**Check:** `mmx nodes give_each_app_server_a_key` lists two fleet stages
for each app server:

```text
Fleet stages
  app_server_keys[web01]/create_key_on_database    db01    → db01
  app_server_keys[web01]/write_key_on_app_server   web01   → web01
  app_server_keys[web02]/create_key_on_database    db01    → db01
  app_server_keys[web02]/write_key_on_app_server   web02   → web02
```

`mmx run give_each_app_server_a_key` then ends with `Run ...: succeeded`,
and `mmx results --fleet-stage 'app_server_keys[web02]'` shows one
iteration, with `outputs: {"key":"[redacted]"}` from `db01`. A dryrun or
run with `--limit web02` still reaches `db01` for `web02`. Before MMX
connects, it lists `db01` under `Nodes outside --limit that help selected
nodes`; `db01` is a helper node, as
[Roll out a change gradually](roll-out-a-change-gradually.md#take-each-server-out-of-the-load-balancer-while-it-changes)
explains.

The loop can publish its own outputs, keyed by the loop's node, such as
`.outputs.app_server_keys.web02.<name>` for later fleet stages.
[Roll out a change gradually](roll-out-a-change-gradually.md#take-each-server-out-of-the-load-balancer-while-it-changes)
covers the loop in full: how many nodes work at once, what a failure stops,
and `mmx resume`. For a list of things that are not nodes, such as
customers' sites, `loop` over fleet stages works in the same way and needs
a `key`.

## Save outputs to files on your controller machine

`save_on_controller_machine`, beside `outputs` on a fleet stage, writes
outputs to files on your controller machine. No node task runs on your
controller machine: `mmx` writes the files after the fleet stage's nodes
finish. For example, this playbook saves each server's network addresses,
and one list of all servers:

```yaml
# playbooks/record_server_addresses.yml
fleet_stages:
  - id: record_addresses
    nodes: all
    node_tasks:
      - id: list_addresses
        shell:
          script: ip -brief address
          run_script_in_dryrun: true
          changed_when: false
    outputs:
      addresses: "{{ .node_tasks.list_addresses.stdout }}"
      default_address: "{{ .node.network.default_ipv4_address }}"
    save_on_controller_machine:
      - output: addresses                          # one file for each node
        path: "server_details/{{ .node.name }}.txt"
        permissions_mode: "0644"
      - template: files/server_addresses.tmpl      # one file for the whole collection
        path: server_details/addresses.txt
        permissions_mode: "0644"
```

The template `files/server_addresses.tmpl` reads the fleet stage's whole
output collection:

```text
{{ range $node, $outputs := .outputs.record_addresses -}}
{{ $node }} {{ $outputs.default_address }}
{{ end -}}
```

**Check:** `mmx explain record_server_addresses` lists each file before
any run. `mmx dryrun record_server_addresses --diff` shows each file as
`would_change` with its content and writes nothing. `mmx run
record_server_addresses` lists the files under `controller machine`:

```text
  record_addresses / controller machine / save_on_controller_machine[0] from web01: created "/home/you/shop/server_details/web01.txt"
    absent -> 174 bytes, permissions_mode 0644, uid 1002, gid 1002
  ...
  record_addresses / controller machine / save_on_controller_machine[1]: created "/home/you/shop/server_details/addresses.txt"
    absent -> 73 bytes, permissions_mode 0644, uid 1002, gid 1002
```

`server_details/addresses.txt` then reads:

```text
db01 203.0.113.21
lb01 203.0.113.31
web01 203.0.113.11
web02 203.0.113.12
```

A second run reports each file `unchanged` and leaves the file alone.

Each entry has `path`, `permissions_mode`, and exactly one of `output` and
`template`:

| Field | What the field does |
| --- | --- |
| `output` | The name of an output in the same fleet stage's `outputs`. MMX writes one file for each node whose fleet stage succeeded, so `path` must contain `{{ .node.name }}`. Text and binary content are written as they are, and a number or a boolean as its text. A list or a mapping needs a `template`. |
| `template` | A template file in the project. MMX renders the template once, from the fleet stage's whole output collection, and writes one file. The template may read the outputs and outcomes of the same fleet stage and earlier ones, variables that every node shares, the inventory, and named secrets. |
| `path` | Where to write. A relative path starts at the project root and can't leave the project. An absolute path may name any place on your controller machine. MMX creates missing directories with mode `0755`. `path` may read variables that every node shares, the inventory, `.node.name` (in an `output` entry), and earlier fleet stages' outputs, but not a node's facts or results, and not a secret. An output, or the item of a loop over outputs, may come from a node, so its value fills part of one file name, such as `hosts/{{ .outputs.inspect[.node.name].hostname }}.yml`: a value with `/`, or a path that then has a `.` or `..` part, fails the file. A node cannot choose to write another file, such as one of your playbooks. |
| `permissions_mode` | The file's mode, such as `"0644"`. Required. |

How MMX writes the files:

- **A `template` file needs every node.** When a selected node failed, and
  the fleet stage's `when` didn't skip that node, the `template` file is
  `blocked`, naming the failed node, and the run fails. The `output` files
  of the nodes that succeeded are still written.
- **A file holds the old content or the new content.** MMX writes each
  file under another name, and then renames the new file into place.
- **`mmx resume` writes the missing files:** the files of nodes that
  succeed now, a `template` file that was blocked, and a file that could
  not be written. A file that the earlier attempt wrote is kept.
- **A `--limit` that narrows a fleet stage with a `template` entry is
  refused,** because the template reads the whole output collection, as
  [Choose which servers a run touches](choose-which-servers-a-run-touches.md#if-something-is-different)
  says:

  ```text
  mmx: save_on_controller_machine: --limit excludes some nodes from fleet stage record_addresses, whose whole output collection fleet stage record_addresses reads; include all output-producing nodes in --limit or omit --limit
  ```

- **A `for_each_node` loop or a `loop` over fleet stages takes no
  `save_on_controller_machine`.** Put the entry on a fleet stage inside the
  loop. A file there that could not be written leaves the iteration
  unfinished, so `mmx resume` writes the file.
- **A `path` that reads an earlier fleet stage's output** names its file
  only during the run, and `mmx explain` says so.
- **`--json`** lists the saved files under `controller_machine_files`.

A secret output can be saved too, such as the key from `db01`. Add the
entry to `prepare_database`:

```yaml
    save_on_controller_machine:
      - output: app_key
        path: "credentials/{{ .node.name }}_app_key"
        permissions_mode: "0600"
```

The key goes from `mmx`'s memory to `credentials/db01_app_key`, and the
run's saved history on your controller machine holds no copy. The result
shows the file's size, not the key. A mode that other accounts can read,
such as `"0644"`, fails the entry by the same rule as for a `file` node
task: [Files that hold secrets](../reference/modules/file.md#files-that-hold-secrets).
A `path` that would contain a secret is refused. Add
`credentials/` to the project's `.gitignore`, so the key doesn't go into
Git.

Module pages that rely on this section: [`read_file`](../reference/modules/read_file.md).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `write_app_config: ... .outputs.prepare_database.db01.address: prepare_database on db01 is failed: read_app_key: /etc/shop/app_key is not a regular file`, and the node is `blocked` | The fleet stage that publishes the output failed on `db01`, so every reader is blocked. Fix the cause on `db01`, here a directory at the key file's path, then run `mmx resume` with the run's ID. |
| `prepare_database / db01: not_checked` and `write_app_config: not_checked` in a dryrun | The value exists only after a run, such as a key file that a run creates. See [What a dryrun shows before a value exists](#what-a-dryrun-shows-before-a-value-exists). Run, and change the playbook only if the run fails. |
| `unknown output in .outputs.prepare_database.db01.adress` | The output name is misspelled, or the fleet stage doesn't declare the output under `outputs`. |
| `reference .outputs.prepare_database.db02.address names a node that the referenced fleet stage does not select` | The node name in the reference is not one of that fleet stage's nodes. `mmx nodes connect_servers` lists each fleet stage's nodes. |
| `reference .outputs.configure_load_balancer.lb01.address names a value that is not declared or is declared later` | The reference names a later fleet stage. Move the fleet stage that publishes the output before the fleet stage that reads the output. |
| `--limit excludes fleet stage prepare_database, whose outputs fleet stage configure_app reads` | The limit leaves out the node that publishes the output. Add the node to the limit, such as `--limit db01,web01`, or omit `--limit`. |
| `dangerously_let_every_account_read_secret: /etc/shop/app.conf holds a secret, and permissions_mode 0644 lets every account on the node read it.` | The file's content holds a secret. Use a mode such as `"0600"`, or see [Files that hold secrets](../reference/modules/file.md#files-that-hold-secrets). |
| `/etc/nginx/sites-enabled/default is a symbolic link, and file manages only regular files` | Remove a link with the `symlink` module and `state: absent`, as `remove_default_site` does, not with `file`. |
| `generate_app_key: skipped, unknown — the shell node task has no dryrun command` | A dryrun doesn't run a `shell` command without `dryrun`. A command that only reads can set `run_script_in_dryrun: true`, so a dryrun runs the script as written. |

## Where to go next

- [Roll out a change gradually](roll-out-a-change-gradually.md): a
  `for_each_node` loop that takes each web server out of the load balancer
  while the web server changes.
- [Choose which servers a run touches](choose-which-servers-a-run-touches.md):
  `--limit`, and a fleet stage's `when` for each server.
- Secrets that start on a node, and how `mmx resume` handles such secrets:
  [Keep secrets](keep-secrets.md#a-secret-that-a-node-makes) and the
  [secrets reference](../reference/secrets.md#secrets-that-start-on-a-node).
- Templates, facts, and template functions:
  [Use variables and templates](use-variables-and-templates.md).
- The [`read_file`](../reference/modules/read_file.md) and
  [`shell`](../reference/modules/shell.md) modules' pages.
- The [rolling-deployment example](../../examples/rolling-deployment/README.md):
  a complete project that passes outputs between the load balancer and the
  application servers.
