# Inventory file reference

This page lists every field of an inventory file, the rules for node and
group names, and how a command chooses its inventory. A **node** is a Linux
server that MMX manages. An **inventory** is a file in `inventories/`, such
as `inventories/example.yml`, that lists the nodes, how MMX logs in to each
node, and the groups of nodes. A **group** is
a name for several nodes, such as `web_servers`, that a playbook can select
instead of each node. The **controller machine** is the computer where you
run `mmx`. To write an inventory step by step, see
[Describe your servers](../how-to/describe-your-servers.md). To prepare a
server for MMX's first login, see
[Connect to your servers](../how-to/connect-to-your-servers.md).

## The inventory file

An inventory that uses every field:

```yaml
# inventories/example.yml
vault_file: secrets/example.age       # the vault for this inventory
vars:                                 # inventory variables: every node in this inventory
  log_level: info

nodes:
  web01:                              # a node name you choose
    address: 203.0.113.11
    ssh_user: root
  web02:
    address: 203.0.113.12
    ssh_user: deploy
    ssh_port: 2222
    ssh_identity_file: ~/.ssh/web02_ed25519
    ssh_known_hosts: ssh/known_hosts
    ssh_config: ssh/config
    ssh_password: "{{ .secrets.deploy_ssh_password }}"
    sudo_password: "{{ .secrets.deploy_sudo_password }}"
    connection: https_with_ssh_fallback
    https_address: https://203.0.113.5:17443
    mmx_runner_download_url: http://203.0.113.40:18473/mmx
    vars:                             # node variables: this node only
      worker_count: 8

groups:
  web_servers:                        # a group name you choose
    nodes: [web01, web02]
    vars:                             # group variables: every node in the group
      worker_count: 4
```

Top-level fields:

| Field | Type | Required | What the field holds |
| --- | --- | --- | --- |
| [`nodes`](#nodes) | mapping of node name to node fields | Yes, at least one node | The nodes and how MMX reaches each node |
| [`groups`](#groups) | mapping of group name to `nodes` and `vars` | No | Named sets of nodes |
| [`vars`](#vars) | mapping | No | Variables for every node in the inventory |
| [`vault_file`](#vault_file) | path | No | The vault for the commands that use this inventory |

Fields of each node, under `nodes`:

| Field | Type | Default |
| --- | --- | --- |
| [`address`](#address) | IP address, host name, or `Host` name from your SSH configuration | Required |
| [`ssh_user`](#ssh_user) | account name | OpenSSH chooses |
| [`ssh_port`](#ssh_port) | integer from 1 to 65535 | OpenSSH chooses, usually 22 |
| [`ssh_identity_file`](#ssh_identity_file) | path on the controller machine | Your SSH agent and your usual keys |
| [`ssh_known_hosts`](#ssh_known_hosts) | path on the controller machine | `~/.ssh/known_hosts` |
| [`ssh_config`](#ssh_config) | path on the controller machine | `~/.ssh/config` |
| [`ssh_password`](#ssh_password) | one secret reference | MMX asks at a terminal when the server needs a password |
| [`sudo_password`](#sudo_password) | one secret reference | MMX asks at a terminal when `sudo` needs a password |
| [`connection`](#connection) | `https_with_ssh_fallback` or `ssh_only` | `https_with_ssh_fallback` |
| [`https_address`](#https_address) | `https://` and a host name, with an optional port | `https://<the node's host name>:7443` |
| [`mmx_runner_download_url`](#mmx_runner_download_url) | HTTP or HTTPS URL | MMX sends `mmx-runner` over SSH |
| [`vars`](#vars) | mapping | No node variables |

**The file's name is the inventory's name.** `inventories/staging.yml` is
the inventory `staging`. A name starts with a letter or underscore and
contains only letters, digits, underscores, or hyphens. Another `.yml` file
in `inventories/`, such as `my.prod.yml`, stops `mmx validate` with
`invalid inventories name "my.prod"`.

**One inventory for each environment.** A command uses exactly one
inventory, and MMX never merges two inventory files. Node and group names
may repeat between inventories, so one playbook works with each inventory.

**The YAML rules of the settings file apply.** An unknown field, such as
`ssh_host`, fails with
`inventories/example.yml: line 4 column 5 (nodes.web01.ssh_host): unknown field`.
[The settings file](project-settings.md#the-settings-file) lists the other
rules, such as `true` and `false` for booleans.

**Checks without a server.** `mmx nodes` lists the nodes and groups of an
inventory, and `mmx validate` checks every inventory of the project. `mmx nodes` and
`mmx validate` read only your files:

```text
$ mmx nodes
Inventory: example (inventories/example.yml, 2 nodes)

Nodes
  web01   203.0.113.11   root     web_servers
  web02   203.0.113.12   deploy   web_servers

Groups
  web_servers   web01, web02
```

`mmx validate` does not check that a file named in `ssh_identity_file`,
`ssh_known_hosts`, or `ssh_config` exists. A missing file shows at the
first connection to the node.

## nodes

`nodes` lists the servers of the inventory. Each key is a node name that
you choose, and the fields below the key say how MMX reaches the node.

```yaml
nodes:
  web01:                       # the node name
    address: 203.0.113.11
    ssh_user: root
```

- **A node name** starts with a letter or underscore and contains only
  letters, digits, underscores, or hyphens. Playbooks, `--limit`, and the
  output use the node name. The node name need not match the server's
  host name. A name such as `web.01` fails with
  `invalid node name "web.01"`.
- **`all` is not a node name,** because a selection of `all` means every
  node of the inventory. A node named `all` fails with
  `node name "all" is reserved: selecting all selects every node of the inventory; give the node another name`.
- **At least one node.** An inventory without nodes fails with
  `inventory requires at least one node`.
- **Two node names for one machine:** each node name must reach a
  different machine. The [When something fails](../when-something-fails.md#two-inventory-names-reach-one-server)
  page lists the error for two names that reach one machine.

### address

`address` is where MMX connects to the node: an IP address, a host name, or
a `Host` name from your SSH configuration.

```yaml
    address: 203.0.113.11
```

- **Required.** An empty address, an address that starts with `-`, or an
  address with a space, a tab, or a line break fails with
  `node address must not be empty, start with '-', or contain spaces, tabs, line breaks, or a NUL byte`.
- **A `Host` name** from `~/.ssh/config` makes MMX use that entry's
  settings, as `ssh` does. HTTPS then goes to the entry's `HostName`.
  [A Host name from your SSH configuration](../how-to/connect-to-your-servers.md#a-host-name-from-your-ssh-configuration)
  shows an example.

### ssh_user

`ssh_user` is the account that MMX logs in as over SSH.

```yaml
    ssh_user: deploy
```

- **The account** must be `root`, or an account that can run any command
  with `sudo`, because MMX installs software and changes system files.
  Otherwise the first connection fails with
  `account deploy does not have unrestricted root sudo access`.
- **Allowed names** start with a letter or underscore and contain only
  letters, digits, underscores, or hyphens. Otherwise the inventory fails
  with
  `ssh_user must start with a letter or underscore and contain only letters, digits, underscores, or hyphens`.
- **Default:** OpenSSH chooses the account, as plain `ssh` does: from a
  `User` line in your SSH configuration, or else your own user name on the
  controller machine. `mmx nodes` then shows `-` in the account column.
- `ssh_user` does not choose the account that node tasks run as. MMX's
  program on the node runs as root.

### ssh_port

`ssh_port` is the node's SSH port, as `ssh -p` gives it.

```yaml
    ssh_port: 2222
```

- **Type:** an integer from 1 to 65535. A number outside the range fails
  with `ssh_port must be a port number from 1 to 65535`, and a quoted
  number, such as `"2222"`, fails with `expected int, got !!str`.
- **Default:** OpenSSH chooses the port: a `Port` line in your SSH
  configuration, or else 22.
- **After you change `ssh_port`** on a node that MMX already manages, read
  [Other SSH settings](../how-to/connect-to-your-servers.md#other-ssh-settings)
  for what the next run does and which key MMX logs in with.
- **To change the port that sshd listens on,** follow
  [Change SSH or network settings safely](../how-to/change-ssh-or-network-settings-safely.md),
  so that MMX does not lock itself out.

### ssh_identity_file

`ssh_identity_file` is the SSH private key that MMX logs in to the node
with, as `ssh -i FILE -o IdentitiesOnly=yes` uses the key.

```yaml
    ssh_identity_file: ~/.ssh/web02_ed25519
```

- **Type:** a path on the controller machine. See
  [Paths on the controller machine](#paths-on-the-controller-machine).
- **Default:** MMX offers the keys that plain `ssh` offers: the keys of
  your SSH agent and your usual key files.
- **With the field set,** MMX offers only the key in `ssh_identity_file`. So the field helps
  when the server answers `Too many authentication failures`, because your
  SSH agent offered other keys first.
- **A key with a passphrase:** MMX asks for the passphrase at a terminal,
  only for a key file that MMX found for this node.

### ssh_known_hosts

`ssh_known_hosts` is the file of trusted SSH host keys that MMX checks the
node's host key against, as `ssh -o UserKnownHostsFile=FILE` does.

```yaml
    ssh_known_hosts: ssh/known_hosts
```

- **Type:** a path on the controller machine. See
  [Paths on the controller machine](#paths-on-the-controller-machine).
- **Default:** `~/.ssh/known_hosts`, as for plain `ssh`.
- **A file in the project,** such as `ssh/known_hosts`, can go into Git,
  so every member of the team trusts the same host keys.
- MMX connects only to a server whose host key the file already trusts,
  and never accepts a new host key.

### ssh_config

`ssh_config` is the SSH configuration file that MMX reads for the node
instead of `~/.ssh/config` and the system's `/etc/ssh/ssh_config`, as
`ssh -F FILE` does.

```yaml
    ssh_config: ssh/config
```

- **Type:** a path on the controller machine. See
  [Paths on the controller machine](#paths-on-the-controller-machine).
- **Default:** `~/.ssh/config` and the system's `/etc/ssh/ssh_config`, as
  for plain `ssh`. With the field set, a system-wide setting no longer
  applies to the node.
- **Use:** settings that the inventory has no field for, such as a jump
  host: [A server behind a jump host](../how-to/connect-to-your-servers.md#a-server-behind-a-jump-host).
- A file that OpenSSH cannot read fails the connection to the node, and
  the error gives OpenSSH's own message.

### Paths on the controller machine

`ssh_identity_file`, `ssh_known_hosts`, and `ssh_config` name files on the
controller machine:

| Written as | Starts at | Example |
| --- | --- | --- |
| `~/` and a path | Your home directory, as for OpenSSH | `~/.ssh/web02_ed25519` is `/home/you/.ssh/web02_ed25519` |
| An absolute path | The root directory | `/home/you/.ssh/web02_ed25519` |
| Another relative path | The project directory, where `mmx_project_settings.yml` is | `ssh/known_hosts` is `<project>/ssh/known_hosts` |

Only `~/` is expanded. `~bob/.ssh/key` is a relative path in the project
directory, not a path in Bob's home directory.

### ssh_password

`ssh_password` gives MMX the node's SSH password from a secret, for a run
without a terminal, such as a CI job.

```yaml
    ssh_password: "{{ .secrets.deploy_ssh_password }}"
```

- **Type:** exactly one secret reference, in quotes, with no text before or
  after the reference. A plain password fails with
  `must be a whole-value secret reference such as "{{ .secrets.ssh_password }}"`.
- **Default:** MMX asks for the password at a terminal when the server
  needs one. Without a terminal, the command fails with
  `SSH password for deploy@203.0.113.12:22 is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)`.
- **The secret** comes from `secrets_from_env` or from the vault, as any
  secret does: [secrets reference](secrets.md#where-a-secrets-value-comes-from).
- MMX keeps the password in memory only, and reads the password only when
  a connection over SSH needs the password.
  [Passwords for automation](../how-to/connect-to-your-servers.md#passwords-for-automation)
  shows the whole setup.

### sudo_password

`sudo_password` gives MMX the `sudo` password of `ssh_user` from a secret,
for a run without a terminal.

```yaml
    sudo_password: "{{ .secrets.deploy_sudo_password }}"
```

- **Type and default:** as for [`ssh_password`](#ssh_password).
- **When MMX needs the password:** at the first contact, and at each
  connection over SSH when the account's `sudo` asks for a password. A run
  over HTTPS needs no `sudo` password. `root`, or an account whose `sudo`
  asks for no password, needs no `sudo_password`.

### connection

`connection` chooses how MMX connects to the node after the first contact.

```yaml
    connection: ssh_only
```

| Value | What MMX does |
| --- | --- |
| `https_with_ssh_fallback` (default) | MMX connects over HTTPS to MMX's service on the node, on port 7443. When port 7443 cannot be reached, MMX does the same work over SSH and prints a notice. |
| `ssh_only` | MMX connects over SSH for every request, with no attempt at HTTPS and no notice. |

- **Another value** fails with
  `connection must be https_with_ssh_fallback or ssh_only`.
- **For one command only,** without a change to the shared inventory,
  pass `--connection ssh_only` to `mmx dryrun`, `mmx run`,
  `mmx run-command`, `mmx check-one-time-playbooks`, or
  `mmx complete-one-time-playbook`. `mmx resume` keeps the run's choice
  and refuses `--connection`.
- **When to choose `ssh_only`:** a firewall closes port 7443, or sshd uses
  login rules that only OpenSSH can check.
  [A server whose port 7443 is closed](../how-to/connect-to-your-servers.md#a-server-whose-port-7443-is-closed)
  and
  [Login rules that only OpenSSH can check](../how-to/connect-to-your-servers.md#login-rules-that-only-openssh-can-check)
  explain each case.

### https_address

`https_address` is where MMX reaches MMX's HTTPS service on the node, when
the service is reachable at another address or port than the node's own
port 7443, such as through a NAT port mapping.

```yaml
    https_address: https://203.0.113.5:17443
```

- **Type:** `https://`, a host name or IP address, an optional port, and
  an optional trailing `/`. Anything else, such as a missing `https://`, a
  user name, a path, a query, or a fragment, fails with
  `https_address must contain https:// and a host name, with an optional port and trailing slash, but no credentials, other path, query, or fragment`.
- **Default:** `https://<host>:7443`, where `<host>` is the host that
  OpenSSH connects to for `address`. For a `Host` name from your SSH
  configuration, `<host>` is the entry's `HostName`.
- **Without a port,** such as `https://203.0.113.5`, MMX connects to port
  443, the standard HTTPS port.
- [HTTPS through a port mapping](../how-to/connect-to-your-servers.md#https-through-a-port-mapping)
  has the setup.

### mmx_runner_download_url

`mmx_runner_download_url` is a web server from which the node downloads
`mmx-runner`, MMX's program for the node, instead of receiving the file
over SSH. The setting helps a large fleet with a web server near the nodes.

```yaml
    mmx_runner_download_url: http://203.0.113.40:18473/mmx
```

- **Type:** an `http://` or `https://` URL, without a user name, password,
  query, or fragment. Otherwise the inventory fails with
  `mmx_runner_download_url must be an HTTP or HTTPS URL without credentials, query, or fragment`.
- **Default:** none. MMX sends `mmx-runner` over the node's SSH connection.
- **What the node downloads:** the URL, then `/mmx-runner-` and the
  SHA-256 hash of the file, such as
  `http://203.0.113.40:18473/mmx/mmx-runner-8bf9a38b...`. The node
  downloads the file with `curl`, so `curl` must be installed on the node.
- **Plain HTTP is safe for the download,** because MMX sends the expected hash over
  verified SSH and installs only a file with that hash.
- [Runner downloads for a large fleet](../how-to/connect-to-your-servers.md#runner-downloads-for-a-large-fleet)
  sets up the web server, and says what happens when a download fails.

## groups

`groups` gives names to sets of nodes, so that a playbook, `--limit`, and
the output can name the set instead of each node.

```yaml
groups:
  web_servers:                 # a group name you choose
    nodes: [web01, web02]
    vars:
      http_port: 8080
  database_servers:
    nodes: [db01]
```

A group has two fields, both optional: `nodes`, the list of node names,
and [`vars`](#vars). A group without `nodes` is empty. Connection
settings, such as `ssh_user`, are written on each node, not on a group.

| Rule | What a break of the rule shows |
| --- | --- |
| A group name starts with a letter or underscore and contains only letters, digits, underscores, or hyphens. | `inventory group "web.servers" must start with a letter or underscore and contain only letters, digits, underscores, or hyphens; the group name must differ from 'all' and every node name` |
| A group name differs from `all` and from every node name of the inventory. | The same message as the rule above, such as for a group `web01` |
| `nodes` lists node names of the same inventory, each name once. A group cannot list another group: MMX has no groups of groups. | `group all_servers: unknown or repeated node web_servers` |
| Two groups of one node must not set the same variable, even to the same value, because MMX never chooses between groups. | `node web02 belongs to groups small_servers and web_servers, which set the same variable worker_count; define the variable in inventory vars, in only one group, or in each node's vars` |

- **A node can be in several groups,** such as `web01` in `web_servers`
  and in `backed_up`.
- **An empty group,** `nodes: []`, is valid. A fleet stage that selects
  only an empty group fails, unless the fleet stage sets
  `skip_when_no_nodes_are_selected: true`:
  [Choose which servers a run touches](../how-to/choose-which-servers-a-run-touches.md).
- **To select two groups together,** a playbook lists both groups, such as
  `nodes: [web_servers, database_servers]`.

## vars

`vars` sets variables, which node tasks read as `{{ .vars.NAME }}`. An
inventory can set `vars` in three places:

| Where | Which nodes get the variables |
| --- | --- |
| At the top of the inventory file | Every node in the inventory |
| In a group | Every node in the group |
| On a node | The node only |

```yaml
vars:                          # every node in this inventory
  log_level: info
nodes:
  web01:
    address: 203.0.113.11
  web02:
    address: 203.0.113.12
    vars:                      # web02 only
      worker_count: 8
groups:
  web_servers:
    nodes: [web01, web02]
    vars:                      # every node in web_servers
      worker_count: 4
```

- **Type:** a mapping. A value can be text, a number, a boolean, a list, or
  a mapping.
- **Order:** the project's `vars` in `mmx_project_settings.yml` come
  first, then the inventory's `vars`, then the `vars` of the node's groups,
  then the node's own `vars`. A later place replaces an earlier value of
  the same name, and a list or a mapping is replaced as a whole. In the
  example, `web02` gets `worker_count: 8`.
  [Describe your servers](../how-to/describe-your-servers.md#steps), step 3,
  walks through the order.
- **A command-line variable** that a playbook declares in
  `command_line_vars` must not be in any `vars`.
- **The variables of another node's groups and of that node itself** are
  readable as `{{ .inventory.nodes.NAME.vars.VARIABLE }}`. Project and
  inventory variables are the same for every node: read them through
  `.vars`.
  [Values about other servers](../how-to/use-variables-and-templates.md#values-about-other-servers)
  shows both.

## vault_file

`vault_file` names the vault that the commands with this inventory use: the
encrypted file of secrets. So a staging inventory can use a vault without
production's secrets.

```yaml
# inventories/staging.yml
vault_file: secrets/staging.age
```

- **Type:** a path. A relative path starts at the directory of the
  inventory file, in the example `inventories/`, so `secrets/staging.age` is
  `inventories/secrets/staging.age`.
- **Default:** the project's `vault_file` in `mmx_project_settings.yml`, or
  else `secrets_vault.age` in the project directory.
- [Which vault file](secrets.md#which-vault-file) lists the order in which
  MMX chooses the vault, and
  [A vault for each inventory](../how-to/keep-secrets.md#a-vault-for-each-inventory)
  sets up one vault for each inventory.

## Choosing the inventory on the command line

A command uses the inventory that `--inventory NAME` names, or else the
project's `default_inventory` from `mmx_project_settings.yml`:

```sh
mmx nodes --inventory staging
mmx run set_up_web_servers --inventory staging
```

- **`--inventory` takes the inventory's name,** not a path. A path fails
  with `--inventory must be a name, not a path: "inventories/example.yml"`.
- **A name without a file** fails with
  `there is no inventory staging: inventories/staging.yml does not exist; the project's inventories are: example`.
- **`mmx validate` without a playbook name** checks every inventory, each
  inventory on its own, with every playbook.
- **The `mmx secrets` commands** take `--inventory NAME` to use the
  inventory's `vault_file`.
- **Commands that pick a saved run,** such as `mmx results` without a run
  ID, pick the latest run of the inventory:
  [Selecting saved runs](commands.md#selecting-saved-runs).
- **A run saves the inventory with the run,** so `mmx resume` uses the
  inventory as the run read the inventory, even after you edit the file.
- [Choose which servers a run touches](../how-to/choose-which-servers-a-run-touches.md)
  covers `--limit` and the other ways to run on only some of the
  inventory's nodes.
