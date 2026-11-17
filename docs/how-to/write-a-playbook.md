# Write a playbook

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you have a project where `mmx run example`
worked ([Get started](../get-started.md)), and you want MMX to set up
something real on your servers.

**What this page does:** you write `playbooks/set_up_web_servers.yml`. It
installs nginx, puts a home page in place, adds an nginx site on port 8080,
reloads nginx only when the site file changed, and checks that the home
page answers.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**, run
  in order. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order.
- A node task names one **module**, the code that does the work, such as
  `apt_package` or `file`, with the module's inputs below its name.
- A **node task block** groups a change with what follows it: making the
  change, putting it into use, and checking it.

## Before you start

- The server runs Debian or Ubuntu, because this playbook installs nginx
  with `apt_package`. On Fedora, Rocky Linux, or AlmaLinux, use
  `dnf_package` with the same inputs; [If something is different](#if-something-is-different)
  covers the other distributions.
- The example playbook from Get started, `playbooks/example.yml`, can stay
  or go. Deleting the file leaves `/etc/mmx-example` on the server; MMX
  removes only what a playbook tells it to remove.
- Find the inputs of any module with `mmx describe-module NAME`, such as
  `mmx describe-module file`. It prints the module's whole page: its
  inputs, an example, its results, and what a dryrun does.

## Steps

1. **Put your server in a group.** Groups let a playbook name a kind of
   server instead of each server. Add a `groups` part to
   `inventories/example.yml`:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root

   groups:
     web_servers:              # a group name you choose
       nodes: [web01]
   ```

   **Check:** `mmx nodes` shows the group beside the server, and in its
   own list:

   ```text
   Nodes
     web01   203.0.113.11   root   web_servers

   Groups
     web_servers   web01
   ```

2. **Start the playbook with a fleet stage that installs nginx.** Create
   `playbooks/set_up_web_servers.yml`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   fleet_stages:
     - id: set_up_nginx          # a fleet stage ID you choose
       nodes: web_servers        # a group or node name from the inventory
       node_tasks:
         - id: install_nginx     # a node task ID you choose; output and results use it
           apt_package:
             packages:
               - {name: nginx, version: newest_when_first_installed}
   ```

   `newest_when_first_installed` installs the newest version the first
   time and then leaves the installed version alone. MMX installs a
   package without starting its services: a later node task starts them.

   The file's name without `.yml` is the playbook's name, which every
   command takes: `mmx run set_up_web_servers`. There is no default
   playbook.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:` and the
   project's name.

3. **Add the home page and its directory.** Add these two node tasks below
   `install_nginx`, at the same indent:

   ```yaml
         - id: create_site_directory
           directory:
             path: /srv/shop/public
             create_missing_parent_directories: true

         - id: write_home_page
           file:
             path: /srv/shop/public/index.html
             permissions_mode: "0644"
             content: |
               <h1>Shop</h1>
   ```

   Reason: a `file` task writes into a directory that must already exist.
   Nothing on a new server creates `/srv/shop/public`, so the playbook
   creates it first. `create_missing_parent_directories: true` also creates
   `/srv/shop`.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:`.

4. **Add the nginx site as a node task block.** Add this below
   `write_home_page`, at the same indent:

   ```yaml
         - task_block:
             id: shop_site
             changes:                  # first, make the change
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   content: |
                     server {
                       listen 8080;
                       root /srv/shop/public;
                     }
             activate:                 # then, put the change into use
               - id: reload_nginx
                 systemd_service:
                   name: nginx
                   start_at_boot: true
                   state: running
                   refresh: reload     # reload nginx only when the site file changed
             verify:                   # then, check that it works
               - id: check_home_page
                 wait_for:
                   url_responds: http://127.0.0.1:8080/
                   response_body_contains: Shop
   ```

   Reason: `systemd_service` in `activate` starts nginx if it is stopped,
   and reloads it only when a file in `changes` changed. It also reloads
   nginx on a later run if an earlier run wrote the file and then stopped
   before the reload. `wait_for` asks the server itself for the home page,
   for up to 60 seconds, and fails the node task block when the page
   doesn't contain `Shop`.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:`. The whole
   file now reads:

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

         - id: create_site_directory
           directory:
             path: /srv/shop/public
             create_missing_parent_directories: true

         - id: write_home_page
           file:
             path: /srv/shop/public/index.html
             permissions_mode: "0644"
             content: |
               <h1>Shop</h1>

         - task_block:
             id: shop_site
             changes:
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   content: |
                     server {
                       listen 8080;
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
                   url_responds: http://127.0.0.1:8080/
                   response_body_contains: Shop
   ```

5. **See what each server will get.**

   ```sh
   mmx explain set_up_web_servers
   ```

   **Check:** for each node task, the output shows every value and where
   it came from: a line of your playbook, or a default:

   ```text
   set_up_nginx → web01
     web01 / set_up_nginx/install_nginx: apt_package (ready)
       at playbooks/set_up_web_servers.yml:6:9 (fleet_stages[0].node_tasks[0])
       apt_package.packages[0].name = "nginx"
         from playbooks/set_up_web_servers.yml:9:22 (fleet_stages[0].node_tasks[0].apt_package.packages[0].name)
       ...
       retry.max_tries = 3
         from MMX default
   ```

6. **Preview the playbook.**

   ```sh
   mmx dryrun set_up_web_servers
   echo "exit code $?"
   ```

   On a server without nginx, the output looks like this:

   ```text
   set_up_nginx / web01 / install_nginx: succeeded, would_change
     [{"name":"nginx-common","to":"1.26.3-3+deb13u10"},{"name":"nginx","to":"1.26.3-3+deb13u10"}]
   set_up_nginx / web01 / create_site_directory: succeeded, would_change
   ...
   set_up_nginx / web01 / write_home_page: would create "/srv/shop/public/index.html"
   set_up_nginx / web01 / shop_site/changes/write_site_file: not_checked, unknown — the parent directory /etc/nginx/conf.d does not exist; create it with an earlier directory node task. The earlier node task install_nginx would change this node first and may provide what is missing; if the run then succeeds, change nothing.
   set_up_nginx / web01 / shop_site/activate/reload_nginx: skipped, unknown — service state can be checked after the node task block's proposed changes are applied
   set_up_nginx / web01 / shop_site/verify/check_home_page: not_checked, unchanged — Get "http://127.0.0.1:8080/": dial tcp 127.0.0.1:8080: connect: connection refused
   exit code 3
   ```

   **Check:** `install_nginx`, `create_site_directory`, and `write_home_page`
   say `would_change`. The site file and the check say `not_checked`, and
   the exit code is 3, while the last lines still say `Run ...: succeeded`:
   the dryrun worked, and exit code 3 says that some work could not be
   checked. That is expected here: the nginx package creates
   `/etc/nginx/conf.d`, and a dryrun installs nothing, so it cannot check
   what depends on the package. Don't add a node task for
   `/etc/nginx/conf.d`. [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains each result.

7. **Run the playbook.**

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** every node task says `succeeded`, the site file says
   `created "/etc/nginx/conf.d/shop.conf"`, and the run ends with
   `Run ...: succeeded` and exit code 0. On a first run, `reload_nginx` says
   `succeeded, changed — service was not running; started`, because MMX installs a
   package without starting its services, so MMX started nginx instead of
   reloading it
   ([Reload or restart](change-a-configuration-and-reload-its-service.md#reload-or-restart)
   says more). If `write_site_file` fails because
   `/etc/nginx/conf.d` is missing after nginx was installed, then add a
   `directory` node task for it before the node task block.

8. **Run it again.**

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** every node task says `unchanged`, and `check_home_page`
   still says `succeeded`.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx validate` prints `line 18 column 11 (fleet_stages[0].node_tasks[2].file.permission_mode): unknown field` | A misspelled input. The message gives the line and the input; `mmx describe-module file` lists the right names, here `permissions_mode`. |
| The server runs Fedora, Rocky Linux, or AlmaLinux | Use `dnf_package` instead of `apt_package`. nginx there reads `/etc/nginx/conf.d/` too. |
| The server runs another distribution | `mmx describe-module` lists every module; find the package module for its package manager. [Limits on each distribution](../reference/limits-on-each-distribution.md) says what is limited where. |
| `check_home_page` fails with `response_body does not contain the required text` | nginx answers, but not with your page. Check `root` in the site file, and that `index.html` holds `Shop`. On port 80, the cause may be Debian's default nginx site: the nginx package enables `/etc/nginx/sites-enabled/default` with `listen 80 default_server`, and that site wins over a site in `/etc/nginx/conf.d/`. Remove the link with a `symlink` node task that has `path: /etc/nginx/sites-enabled/default` and `state: absent`. The `file` module removes only regular files, so `file` cannot remove the link. This page uses port 8080 for that reason. |
| You want to change several files and reload once | Put every file in the same node task block's `changes`; `activate` then reloads once when any of them changed. |

## Where to go next

- [Preview and run changes](preview-and-run-changes.md): what each result
  and exit code means, and how to run on some servers only.
- Node task blocks in full: checking a new file before it replaces the
  live one, checks before a restart, and putting things back when a block
  fails: [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md).
- Variables, templates, and facts about each server:
  [Use variables and templates](use-variables-and-templates.md).
- Roles, for reusing node tasks:
  [Reuse work with roles](reuse-work-with-roles.md).
- Whole example projects: [`examples/`](../../examples/).
