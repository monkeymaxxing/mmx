# Describe your servers

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you have a project from
[Get started](../get-started.md), and you want MMX to know all your
servers: their addresses, the groups they belong to, the values that differ
between servers, and separate server lists for production and staging.

**What this page does:** you list three servers in an inventory file, put
the servers in groups, give the servers variables, and add a second
inventory for a staging environment. `mmx nodes` shows the result of each
step without contacting a server.

**Terms:**

- A **node** is a Linux server that MMX manages.
- An **inventory** is a file in `inventories/`, such as
  `inventories/example.yml`. The inventory lists your nodes, how MMX logs
  in to each node over SSH, and the groups of nodes.
- A **group** is a name for several nodes, such as `web_servers`. A
  playbook can name a group instead of each node.
- A **variable** is a named value, such as `http_port: 8080`, that node
  tasks read as `{{ .vars.http_port }}`. A **node task** is one step that a
  node does, listed in a playbook.

## Before you start

- You have a project directory with `mmx_project_settings.yml`, made with
  `mmx init` as in [Get started](../get-started.md).
- You can log in to each server with `ssh`, as `root` or as an account that
  can run any command with `sudo`, and your computer trusts each server's
  SSH host key. [Connect to your servers](connect-to-your-servers.md)
  covers passwords, other SSH ports, jump hosts, and the first login.

The examples use the servers `web01` at `203.0.113.11`, `web02` at
`203.0.113.12`, and `db01` at `203.0.113.21`. Put in your own servers'
addresses and accounts.

## Steps

1. **List each server under `nodes`.** Replace the contents of
   `inventories/example.yml`:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:                    # a node name you choose
       address: 203.0.113.11   # an IP address, a host name, or a Host name from ~/.ssh/config
       ssh_user: root          # the account MMX logs in as
     web02:
       address: 203.0.113.12
       ssh_user: root
     db01:
       address: 203.0.113.21
       ssh_user: root
   ```

   The node name is the name that playbooks, `--limit`, and the output use.
   You choose the node name, and the node name need not match the
   server's host name.
   `ssh_user` must be `root` or an account that can run any command with
   `sudo`, because MMX installs software and changes system files.
   Without `ssh_user`, OpenSSH chooses the account, as plain `ssh` does:
   from `~/.ssh/config`, or else your own user name.

   **Check:** `mmx nodes` lists the three servers, sorted by name.
   `mmx nodes` reads your files and contacts no server.

   ```text
   Inventory: example (inventories/example.yml, 3 nodes)

   Nodes
     db01    203.0.113.21   root
     web01   203.0.113.11   root
     web02   203.0.113.12   root
   ```

   If `mmx nodes` prints an error instead, the error names the file, line,
   and field to fix.

2. **Put the servers in groups.** Add a `groups` part below `nodes`:

   ```yaml
   # inventories/example.yml, below nodes
   groups:
     web_servers:              # a group name you choose
       nodes: [web01, web02]
     database_servers:
       nodes: [db01]
   ```

   A group lists node names only. A node can be in several groups, such as
   a group `backed_up` with `[web01, db01]`. A group cannot list another
   group: MMX has no groups of groups. To select two groups together, a
   playbook lists both, such as `nodes: [web_servers, database_servers]`.
   The name `all` always means every node in the inventory, so no group
   may be called `all`.

   **Check:** `mmx nodes` shows each node's groups, and each group's nodes:

   ```text
   Nodes
     db01    203.0.113.21   root   database_servers
     web01   203.0.113.11   root   web_servers
     web02   203.0.113.12   root   web_servers

   Groups
     database_servers   db01
     web_servers        web01, web02
   ```

3. **Give the servers variables.** A node's variables can be set in four
   places. A value that changes with each run, such as a release version,
   comes from the command line instead, as the last link on this page
   explains.
   Set each value at the widest place where the value is true. Here, the
   site name is the same everywhere, the web servers share a port, and
   `web02` has more memory, so `web02` gets more workers:

   ```yaml
   # mmx_project_settings.yml
   project_name: mmx_example
   default_inventory: example
   vars:                       # project variables: the same in every inventory
     site_name: shop           # a variable name you choose
     log_level: warn
   ```

   ```yaml
   # inventories/example.yml
   vars:                       # inventory variables: every node in this inventory
     log_level: info

   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root
     web02:
       address: 203.0.113.12
       ssh_user: root
       vars:                   # node variables: this node only
         worker_count: 8
     db01:
       address: 203.0.113.21
       ssh_user: root

   groups:
     web_servers:
       nodes: [web01, web02]
       vars:                   # group variables: every node in the group
         http_port: 8080
         worker_count: 4
     database_servers:
       nodes: [db01]
   ```

   A later place replaces an earlier value of the same name; the
   [`vars` reference](../reference/inventory-file.md#vars) gives the order.
   So `log_level` is `info`, because the inventory replaces the project's
   `warn`. `web01` gets `worker_count: 4` from `web_servers`, and `web02`
   gets `8` from its own `vars`.

   **Check:** write a small playbook that shows the values:

   ```yaml
   # playbooks/show_settings.yml
   fleet_stages:
     - id: show_web_settings
       nodes: web_servers
       node_tasks:
         - id: show_the_settings
           show: "{{ .vars.site_name }}: port {{ .vars.http_port }}, {{ .vars.worker_count }} workers, log level {{ .vars.log_level }}"
   ```

   Then `mmx explain show_settings --limit web01` shows the value for
   `web01`, and the file and line that each part came from. `mmx explain`
   contacts no server.

   ```text
   show_web_settings → web01
     web01 / show_web_settings/show_the_settings: show (ready)
       at playbooks/show_settings.yml:6:9 (fleet_stages[0].node_tasks[0])
       show = "shop: port 8080, 4 workers, log level info"
         from playbooks/show_settings.yml:7:15 (fleet_stages[0].node_tasks[0].show)
         value from inventories/example.yml:22:18 (groups.web_servers.vars.http_port)
         value from inventories/example.yml:3:14 (vars.log_level)
         value from mmx_project_settings.yml:5:14 (vars.site_name)
         value from inventories/example.yml:23:21 (groups.web_servers.vars.worker_count)
   ```

   `mmx run show_settings` shows each server's own values:

   ```text
     show_web_settings / web01 / show_the_settings: succeeded, unchanged
       "shop: port 8080, 4 workers, log level info"
     show_web_settings / web02 / show_the_settings: succeeded, unchanged
       "shop: port 8080, 8 workers, log level info"
   ```

   If a value is not the one you expect, the `value from` line names the
   file and line that set the value.

4. **Add an inventory for a second environment.** A second environment,
   such as staging, is a second inventory file with its own servers and
   values. Create `inventories/staging.yml`:

   ```yaml
   # inventories/staging.yml
   vars:
     log_level: debug

   nodes:
     web01:
       address: 203.0.113.51
       ssh_user: root

   groups:
     web_servers:
       nodes: [web01]
       vars:
         http_port: 8080
         worker_count: 2
   ```

   The inventory's name is the file name without `.yml`: `staging`. Node and
   group names may repeat between inventories, so one playbook with
   `nodes: web_servers` works with both inventories. A command that runs or shows a
   playbook uses exactly one inventory, and MMX never merges two inventory
   files. Project variables
   from `mmx_project_settings.yml` apply in every inventory.

   **Check:** `mmx nodes --inventory staging` lists the staging server:

   ```text
   Inventory: staging (inventories/staging.yml, 1 node)

   Nodes
     web01   203.0.113.51   root   web_servers

   Groups
     web_servers   web01
   ```

5. **Choose the inventory for each command.** `--inventory` takes the
   inventory's name, not a file path:

   ```sh
   mmx run show_settings --inventory staging
   ```

   Without `--inventory`, a command uses `default_inventory` from
   `mmx_project_settings.yml`, here `example`. Choose a default that is
   safe to run by mistake: with `default_inventory: staging`, a command
   that leaves out `--inventory` reaches staging, and production needs
   `--inventory example`.

   **Check:** the header of the run names the inventory, and the output
   shows the staging values:

   ```text
   Project: mmx_example   Playbook: show_settings
   Inventory: staging (1 node)   Selected: 1 node
   ...
     show_web_settings / web01 / show_the_settings: succeeded, unchanged
       "shop: port 8080, 2 workers, log level debug"
   ```

6. **Check every inventory at once.**

   ```sh
   mmx validate
   ```

   Without a playbook name, `mmx validate` reads every inventory file, each
   on its own, and every playbook.

   **Check:** the command prints `Valid: mmx_example` and exits with code 0.
   Otherwise the message names the file, line, and field to fix; see
   [If something is different](#if-something-is-different).

## The inventory's fields

[The inventory file reference](../reference/inventory-file.md) lists every
field of an inventory, a group, and a node. `mmx nodes --json` prints the
inventory for a script or a coding agent; its fields are in
[JSON of mmx nodes](../reference/commands.md#json-of-mmx-nodes).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `group all_servers: unknown or repeated node web_servers` | A group lists another group, or a name that is not a node. Groups list node names only. List the nodes, or let the playbook name both groups. |
| `inventory group "web01" must start with a letter or underscore and contain only letters, digits, underscores, or hyphens; the group name must differ from 'all' and every node name` | A group has a node's name, or the name `all`, or a character that is not allowed. Rename the group. |
| `node web02 belongs to groups small_servers and web_servers, which set the same variable worker_count; define the variable in inventory vars, in only one group, or in each node's vars` | Two groups of one node set the same variable, even to the same value. MMX does not choose between groups. Do what the message says. |
| `reference .vars.http_port is declared for node web01 but not for node db01` | A node task reads a variable that the node does not get, such as `http_port` on `db01`, which is not in `web_servers`. Set the variable for that node, or select only nodes that have the variable. |
| `there is no inventory production: inventories/production.yml does not exist; the project's inventories are: example, staging` | `--inventory` or `default_inventory` names a file that does not exist. Use one of the names that the message lists. |
| `mmx nodes` shows `-` where the account should be | The node has no `ssh_user`. OpenSSH chooses the account, such as from a `User` line in `~/.ssh/config`. |

## Where to go next

- [Connect to your servers](connect-to-your-servers.md): the first login,
  passwords, jump hosts, and what to do when MMX cannot reach a server.
- [Write a playbook](write-a-playbook.md): give your groups real work.
- [Use variables and templates](use-variables-and-templates.md):
  values that change with each run, such as a release version.
- [Choose which servers a run touches](choose-which-servers-a-run-touches.md):
  `--limit` and other ways to run a playbook on only some servers.
