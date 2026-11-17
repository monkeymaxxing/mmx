# Change a configuration and reload its service

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you change a configuration file that a running
service reads, such as an nginx site file, and you want MMX to check the
new configuration, reload or restart the service only when a file changed,
and put the old file back when something fails.

**What this page does:** you grow the nginx site from
[Write a playbook](write-a-playbook.md). nginx checks its configuration
before every reload. Two files that nginx reads together change with one
reload. A broken change is put back: MMX doesn't undo what `changes`
wrote, so the playbook keeps a copy of the working files and puts the copy
back in `on_failure` (steps 5 and 6). A sudo rule is checked before it
replaces the live file. A memcached restart, which empties its cache, waits
for your approval.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** lists **fleet
  stages**. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order.
- A **node task block** (`task_block` in YAML) groups a change with what
  follows it, in phases that run in a fixed order on the node:
  `checks_before_restart_or_reload` when a restart or reload is due, then
  `changes`, `validate`, `activate`, `verify`, `on_failure`, and `cleanup`.
  [The phases in order](#the-phases-in-order) lists every phase.
- A **tracked input** is something that a node task block's `changes`
  phase writes or installs, such as a file or a package. MMX remembers the
  tracked inputs that each service last loaded, so MMX knows when a reload
  or a restart is due.

| What you need | Where |
| --- | --- |
| Check the whole configuration before the service loads the configuration | [Step 1](#steps) |
| Change several files with one reload | [Steps 2 to 4](#steps) |
| Put the working files back when the change fails | [Steps 5 and 6](#steps) |
| Check one file by itself before it replaces the live file | [Step 7](#steps) |
| Make a restart wait for your approval | [Steps 8 and 9](#steps) |
| Know when MMX reloads or restarts | [Reload or restart](#reload-or-restart) |
| A run stopped before the reload | [A run that stopped before the reload](#a-run-that-stopped-before-the-reload) |

## Before you start

- `playbooks/set_up_web_servers.yml` is the playbook from
  [Write a playbook](write-a-playbook.md), and `mmx run set_up_web_servers`
  succeeds on `web01`. The server `web01` is in the group `web_servers` in
  `inventories/example.yml`.
- The server runs Debian or Ubuntu, because the examples install packages
  with `apt_package`.
- Step 7 runs `visudo`, which the `sudo` package installs. If `visudo` is
  missing on the server, add `{name: sudo, version: newest_when_first_installed}`
  to the packages of `install_nginx`.
- Find the inputs of any module with `mmx describe-module NAME`, such as
  `mmx describe-module systemd_service`.

## Steps

1. **Check nginx's whole configuration before nginx loads it.** In
   `playbooks/set_up_web_servers.yml`, add a `validate` phase to the node
   task block `shop_site`, between `changes` and `activate`:

   ```yaml
             validate:                 # check the result of changes, before activate
               - id: check_nginx_configuration
                 shell:
                   script: nginx -t
                   dryrun: nginx -t    # a dryrun runs only this read-only command
   ```

   Reason: `nginx -t` reads every file that nginx reads and fails on any
   error. When a node task in `validate` fails, the node task block stops
   before `activate`, so MMX doesn't reload or start nginx with a
   configuration that nginx rejects.

   **Check:** `mmx run set_up_web_servers` ends with `Run ...: succeeded`,
   and the run shows the check:

   ```text
     set_up_nginx / web01 / shop_site/validate/check_nginx_configuration: succeeded, unchanged
   ```

   The `validate` phase runs on every run, also when no file changed.

2. **Change two files that nginx reads together.** The shop gets its own
   access log, in a log format defined in a second file. Replace the
   `changes` phase of `shop_site` with:

   ```yaml
             changes:
               - id: write_log_format_file
                 file:
                   path: /etc/nginx/conf.d/shop-log-format.conf
                   permissions_mode: "0644"
                   content: |
                     log_format shop '$remote_addr "$request" $status';
               - id: write_site_file
                 file:
                   path: /etc/nginx/conf.d/shop.conf
                   permissions_mode: "0644"
                   content: |
                     server {
                       listen 8080;
                       root /srv/shop/public;
                       access_log /var/log/nginx/shop-access.log shop;
                     }
   ```

   Reason: each file in one node task block's `changes` is a tracked input
   of the block's `activate` phase. `reload_nginx` reloads nginx once when
   any of the files changed. `shop.conf` uses the format `shop` that the
   other file defines, so only `nginx -t` on both files together can check
   the two files.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:` and the
   project's name.

3. **Preview the change.**

   ```sh
   mmx dryrun set_up_web_servers --diff
   echo "exit code $?"
   ```

   ```text
     set_up_nginx / web01 / shop_site/changes/write_log_format_file: succeeded, would_change
     set_up_nginx / web01 / shop_site/changes/write_log_format_file: would create "/etc/nginx/conf.d/shop-log-format.conf"
       absent -> 51 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -0,0 +1,1 @@
       +log_format shop '$remote_addr "$request" $status';
     set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, would_change
     set_up_nginx / web01 / shop_site/changes/write_site_file: would update "/etc/nginx/conf.d/shop.conf"
       51 bytes, permissions_mode 0644, uid 0, gid 0 -> 101 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,4 +1,5 @@
        server {
          listen 8080;
          root /srv/shop/public;
       +  access_log /var/log/nginx/shop-access.log shop;
        }
     set_up_nginx / web01 / shop_site/validate/check_nginx_configuration: succeeded, unchanged
     set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, would_change — tracked inputs /etc/nginx/conf.d/shop-log-format.conf and 1 more changed after the service last started or reloaded
     set_up_nginx / web01 / shop_site/verify/check_home_page: succeeded, unchanged
     set_up_nginx / web01 / shop_site: succeeded, would_change
   exit code 2
   ```

   The `reload_nginx` line says that a run would reload nginx, and why. A
   dryrun writes no file, so `check_nginx_configuration` checked the files
   that are on the server now, not the new ones. A dryrun therefore can't
   find an error in the new files: only the run's `validate` phase or
   `check_new_file` ([step 7](#steps)) can.

   **Check:** exit code 2: something would change. The two files say
   `would_change`, and `reload_nginx` says `would_change`.

4. **Run the change.**

   ```sh
   mmx run set_up_web_servers
   ```

   ```text
     set_up_nginx / web01 / shop_site/changes/write_log_format_file: succeeded, changed
     set_up_nginx / web01 / shop_site/changes/write_log_format_file: created "/etc/nginx/conf.d/shop-log-format.conf"
       absent -> 51 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, changed
     set_up_nginx / web01 / shop_site/changes/write_site_file: updated "/etc/nginx/conf.d/shop.conf"
       51 bytes, permissions_mode 0644, uid 0, gid 0 -> 101 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_nginx / web01 / shop_site/validate/check_nginx_configuration: succeeded, unchanged
     set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, changed — tracked inputs /etc/nginx/conf.d/shop-log-format.conf and 1 more changed after the service last started or reloaded
     set_up_nginx / web01 / shop_site/verify/check_home_page: succeeded, unchanged
     set_up_nginx / web01 / shop_site: succeeded, changed
   Run 432882c1: succeeded
   ```

   **Check:** both files say `changed`, `reload_nginx` says `changed` once,
   and the run ends with `Run ...: succeeded`. A second run says
   `unchanged` for `reload_nginx`: nginx already runs the files on disk.

5. **Keep a copy of the working files, and put the copy back when the
   block fails.** A failed `validate` leaves the new files on disk, because
   MMX doesn't undo what `changes` wrote. nginx keeps running the old
   configuration, but nginx would fail at its next start, such as after a
   reboot. On a server's first deployment there is no working file to
   copy, so a broken first change stays on disk until you fix it and run
   again. Add `save_working_files` as the first node task of `changes`:

   ```yaml
             changes:
               - id: save_working_files
                 shell:
                   script: |
                     set -eu
                     rm -rf /var/backups/shop-nginx
                     mkdir /var/backups/shop-nginx
                     cp -p /etc/nginx/conf.d/shop*.conf /var/backups/shop-nginx/
                   dryrun: test -w /var/backups
                   run_only_if_this_path_exists: /etc/nginx/conf.d/shop.conf
                   changed_when: false
               - id: write_log_format_file
                 ...
   ```

   Then add `on_failure` and `cleanup` at the end of `shop_site`, below
   `verify`:

   ```yaml
             on_failure:               # runs only when an earlier phase failed
               - id: put_back_working_files
                 shell:
                   script: cp -p /var/backups/shop-nginx/*.conf /etc/nginx/conf.d/
                   run_only_if_this_path_exists: /var/backups/shop-nginx
                   skip_if_this_command_succeeds: nginx -t -q
             cleanup:                  # runs last, after success or failure
               - id: remove_saved_files
                 shell:
                   script: rm -rf /var/backups/shop-nginx
                   run_only_if_this_path_exists: /var/backups/shop-nginx
                   changed_when: false
   ```

   What each setting does:

   - `run_only_if_this_path_exists: /etc/nginx/conf.d/shop.conf` skips the
     copy on a new server, where no site file exists yet.
   - `changed_when: false` makes the copy report `unchanged`, so a run that
     changes no configuration file reports the block as `unchanged`.
   - `dryrun: test -w /var/backups` is what a dryrun runs instead of the
     script: the dryrun checks that the copy can be made, and writes nothing.
   - `skip_if_this_command_succeeds: nginx -t -q` puts the copy back only
     when the files on disk fail nginx's check. After a failed `verify`,
     nginx already runs the new files, and those files pass the check, so
     MMX leaves the new files in place.
   - `cleanup` removes the copy after every run of the block.

   **Check:** `mmx run set_up_web_servers` ends with `Run ...: succeeded`,
   and `save_working_files` and `remove_saved_files` both say
   `succeeded, unchanged`.

6. **Test the safety net with a broken change.** In `shop.conf`, change
   the format name `shop` to `shop_log`, a format that no file defines.
   Run `mmx run set_up_web_servers`:

   ```text
   set_up_nginx / web01: failed
     shop_site: shop_site/validate/check_nginx_configuration: required validation did not pass: exit status 1
     ...
     set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, changed
     set_up_nginx / web01 / shop_site/changes/write_site_file: updated "/etc/nginx/conf.d/shop.conf"
       101 bytes, permissions_mode 0644, uid 0, gid 0 -> 105 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_nginx / web01 / shop_site/validate/check_nginx_configuration: failed, unchanged — required validation did not pass: exit status 1
       exit code: 1
       2026/11/15 04:17:44 [emerg] 1977#1977: unknown log format "shop_log" in /etc/nginx/conf.d/shop.conf:4
       nginx: configuration file /etc/nginx/nginx.conf test failed
     set_up_nginx / web01 / shop_site/on_failure/put_back_working_files: succeeded, changed
     set_up_nginx / web01 / shop_site/cleanup/remove_saved_files: succeeded, unchanged
     set_up_nginx / web01 / shop_site: failed, changed — shop_site/validate/check_nginx_configuration: required validation did not pass: exit status 1
   Run dd4150c2: failed
   ```

   The node task block wrote the broken `shop.conf`, and nginx's own
   message names the line. MMX skipped `activate` and `verify`, so nginx
   was not reloaded. `put_back_working_files` then put the working files
   back, and `remove_saved_files` removed the copy. A dryrun before this
   run reports `would_change` and exit code 2, not the error, as
   [step 3](#steps) says.

   **Check:** on the server, nginx's check passes, and the site still
   answers:

   ```sh
   ssh root@203.0.113.11 nginx -t
   ssh root@203.0.113.11 curl -s http://127.0.0.1:8080/
   ```

   ```text
   nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
   nginx: configuration file /etc/nginx/nginx.conf test is successful
   <h1>Shop</h1>
   ```

   Change `shop_log` back to `shop` and run again. `write_site_file` and
   `reload_nginx` say `unchanged`, because the file on the server is again
   the file that nginx runs with.

7. **Check one file by itself before it replaces the live file.** Some
   programs can check one file alone, such as `visudo` for a sudo rule. A
   broken sudo rule can stop every use of sudo on the server. Add these two
   node tasks above the node task block `shop_site`. The rule lets the
   account `deploy` reload nginx:

   ```yaml
         - id: create_deploy_account
           user:
             name: deploy
             create_home_directory: true

         - id: let_deploy_reload_nginx
           file:
             path: /etc/sudoers.d/shop-deploy
             permissions_mode: "0440"
             content: |
               deploy ALL=(root) NOPASSWD: /usr/bin/systemctl reload nginx
             check_new_file:           # check the new file before it replaces the live one
               shell:
                 script: visudo -cf "$NEW_FILE"
                 dryrun: visudo -cf "$NEW_FILE"
                 env:
                   NEW_FILE: "{{ .new_file.path }}"
   ```

   On the node, `file` writes the new content to a temporary file first.
   `check_new_file` runs `visudo` on the temporary file, whose path is
   `{{ .new_file.path }}`. Only when `visudo` exits with 0 does the
   temporary file replace `/etc/sudoers.d/shop-deploy`. A rejected file
   fails the node task, and the live file stays as it was. The path goes
   through `env`, because MMX does not replace `{{ ... }}` references in
   the `dryrun` command.

   `check_new_file` also works in a dryrun, unlike `validate`: the dryrun
   prepares the new file and runs the `dryrun` command on the new file. A
   run checks the new file also when the live file needs no change. `file`,
   `download`, `line`, and `edit_file` accept `check_new_file`.

   **Check:** `mmx dryrun set_up_web_servers` shows the check, with exit
   code 2:

   ```text
     set_up_nginx / web01 / let_deploy_reload_nginx: succeeded, would_change
       check_new_file: succeeded
     set_up_nginx / web01 / let_deploy_reload_nginx: would create "/etc/sudoers.d/shop-deploy"
       absent -> 60 bytes, permissions_mode 0440, uid 0, gid 0
   ```

   Then `mmx run set_up_web_servers` says
   `created "/etc/sudoers.d/shop-deploy"` and ends with
   `Run ...: succeeded`. A rule without the colon after `NOPASSWD` fails
   as [If something is different](#if-something-is-different) shows.

8. **Make a restart that empties a cache wait for your approval.** The
   shop gets memcached, an in-memory cache. memcached can't reload its
   configuration, and a restart empties the cache. Add a command-line
   variable at the top of the playbook, above `fleet_stages`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   command_line_vars:
     restart_approved:                 # set with --var restart_approved=true
       type: boolean
       default: false
   ```

   Then add a second fleet stage at the end of the file:

   ```yaml
     - id: set_up_cache
       nodes: web_servers
       node_tasks:
         - task_block:
             id: shop_cache
             changes:
               - id: install_memcached
                 apt_package:
                   packages:
                     - {name: memcached, version: newest_when_first_installed}
               - id: write_memcached_config
                 file:
                   path: /etc/memcached.conf
                   permissions_mode: "0644"
                   content: |
                     -m 64
                     -p 11211
                     -u memcache
                     -l 127.0.0.1
             checks_before_restart_or_reload:
               - id: require_restart_approval
                 assert:
                   that: "{{ .vars.restart_approved }}"
                   message: >-
                     memcached must restart to load this change, and a restart
                     empties the shop's cache. Run again off-peak with
                     --var restart_approved=true.
             activate:
               - id: restart_memcached
                 systemd_service:
                   name: memcached
                   start_at_boot: true
                   state: running
                   refresh: restart    # memcached can't reload
             verify:
               - id: check_memcached_port
                 wait_for:
                   tcp_port_accepts_connections: 11211
   ```

   `checks_before_restart_or_reload` runs only when the block would restart
   or reload a service that is running now, or run a `shell` node task with
   `only_when_tracked_inputs_changed: true`. Normally the phase runs before
   the block's first write, so a failed check leaves the server as it was.
   When MMX can't tell before the writes that a restart is due, the checks
   run just before the restart instead, after `changes` wrote its files. A
   stopped service starts without the check. The package `memcached` is a
   tracked input too: if you change `install_memcached` to install another
   version, memcached restarts, and the restart needs the approval.

   **Check:** `mmx run set_up_web_servers` installs and starts memcached
   without asking for the approval, because memcached was not running yet:

   ```text
     set_up_cache / web01 / shop_cache/activate/restart_memcached: succeeded, changed — service was not running; started
     set_up_cache / web01 / shop_cache/checks_before_restart_or_reload/require_restart_approval: skipped, unchanged — not needed: this task block restarted or reloaded no running service and ran no command that follows a change
     set_up_cache / web01 / shop_cache/verify/check_memcached_port: succeeded, unchanged — TCP port 127.0.0.1:11211 accepts connections
   ```

9. **Change memcached's memory size, and approve the restart.** Change
   `-m 64` to `-m 128`, then run:

   ```sh
   mmx run set_up_web_servers
   ```

   ```text
   set_up_nginx / web01: succeeded
   set_up_cache / web01: failed
     shop_cache: shop_cache/checks_before_restart_or_reload/require_restart_approval: memcached must restart to load this change, and a restart empties the shop's cache. Run again off-peak with --var restart_approved=true.
     ...
   Run 3aca457e: failed
   ```

   MMX refused the restart and wrote nothing: `/etc/memcached.conf` still
   says `-m 64`. The check doesn't ask you a question, because a run may
   have nobody in front of it. When the time is right, run with the
   approval:

   ```sh
   mmx run set_up_web_servers --var restart_approved=true
   ```

   ```text
     set_up_cache / web01 / shop_cache/checks_before_restart_or_reload/require_restart_approval: succeeded, unchanged
     set_up_cache / web01 / shop_cache/changes/install_memcached: succeeded, unchanged
     set_up_cache / web01 / shop_cache/changes/write_memcached_config: succeeded, changed
     set_up_cache / web01 / shop_cache/changes/write_memcached_config: updated "/etc/memcached.conf"
       40 bytes, permissions_mode 0644, uid 0, gid 0 -> 41 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_cache / web01 / shop_cache/activate/restart_memcached: succeeded, changed — tracked input /etc/memcached.conf changed after the service last started or reloaded
     set_up_cache / web01 / shop_cache/verify/check_memcached_port: succeeded, unchanged — TCP port 127.0.0.1:11211 accepts connections
   Run 2ab46abf: succeeded
   ```

   The approval is on the command line, so the approval is in your shell
   history and in the saved run. A dryrun also runs the check, so
   `mmx dryrun set_up_web_servers` without the approval fails with the same
   message and exit code 1.

   **Check:** `ssh root@203.0.113.11 ps -o args= -C memcached` prints
   `/usr/bin/memcached -m 128 -p 11211 -u memcache -l 127.0.0.1`. A
   later `mmx run set_up_web_servers` without `--var` succeeds, and
   `require_restart_approval` says `skipped`, because nothing needs a
   restart.

## The whole playbook

After step 9, `playbooks/set_up_web_servers.yml` reads:

```yaml
# playbooks/set_up_web_servers.yml
command_line_vars:
  restart_approved:
    type: boolean
    default: false

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

      - id: create_deploy_account
        user:
          name: deploy
          create_home_directory: true

      - id: let_deploy_reload_nginx
        file:
          path: /etc/sudoers.d/shop-deploy
          permissions_mode: "0440"
          content: |
            deploy ALL=(root) NOPASSWD: /usr/bin/systemctl reload nginx
          check_new_file:
            shell:
              script: visudo -cf "$NEW_FILE"
              dryrun: visudo -cf "$NEW_FILE"
              env:
                NEW_FILE: "{{ .new_file.path }}"

      - task_block:
          id: shop_site
          changes:
            - id: save_working_files
              shell:
                script: |
                  set -eu
                  rm -rf /var/backups/shop-nginx
                  mkdir /var/backups/shop-nginx
                  cp -p /etc/nginx/conf.d/shop*.conf /var/backups/shop-nginx/
                dryrun: test -w /var/backups
                run_only_if_this_path_exists: /etc/nginx/conf.d/shop.conf
                changed_when: false
            - id: write_log_format_file
              file:
                path: /etc/nginx/conf.d/shop-log-format.conf
                permissions_mode: "0644"
                content: |
                  log_format shop '$remote_addr "$request" $status';
            - id: write_site_file
              file:
                path: /etc/nginx/conf.d/shop.conf
                permissions_mode: "0644"
                content: |
                  server {
                    listen 8080;
                    root /srv/shop/public;
                    access_log /var/log/nginx/shop-access.log shop;
                  }
          validate:
            - id: check_nginx_configuration
              shell:
                script: nginx -t
                dryrun: nginx -t
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
          on_failure:
            - id: put_back_working_files
              shell:
                script: cp -p /var/backups/shop-nginx/*.conf /etc/nginx/conf.d/
                run_only_if_this_path_exists: /var/backups/shop-nginx
                skip_if_this_command_succeeds: nginx -t -q
          cleanup:
            - id: remove_saved_files
              shell:
                script: rm -rf /var/backups/shop-nginx
                run_only_if_this_path_exists: /var/backups/shop-nginx
                changed_when: false

  - id: set_up_cache
    nodes: web_servers
    node_tasks:
      - task_block:
          id: shop_cache
          changes:
            - id: install_memcached
              apt_package:
                packages:
                  - {name: memcached, version: newest_when_first_installed}
            - id: write_memcached_config
              file:
                path: /etc/memcached.conf
                permissions_mode: "0644"
                content: |
                  -m 128
                  -p 11211
                  -u memcache
                  -l 127.0.0.1
          checks_before_restart_or_reload:
            - id: require_restart_approval
              assert:
                that: "{{ .vars.restart_approved }}"
                message: >-
                  memcached must restart to load this change, and a restart
                  empties the shop's cache. Run again off-peak with
                  --var restart_approved=true.
          activate:
            - id: restart_memcached
              systemd_service:
                name: memcached
                start_at_boot: true
                state: running
                refresh: restart
          verify:
            - id: check_memcached_port
              wait_for:
                tcp_port_accepts_connections: 11211
```

## Reload or restart

A `systemd_service` node task in a node task block's `activate` phase
with `state: running` starts the service if the service is stopped. MMX
installs a package without starting its services, so on a server's first
run the node task starts the service instead of reloading it, and says
`service was not running; started`. On a running service, MMX
compares the tracked inputs of the block with what the service last loaded:

| What changed in `changes` | What MMX does to a running service |
| --- | --- |
| Nothing | Nothing. |
| A file, such as one from `file`, `line`, or `edit_file` | What `refresh` says: `reload` or `restart`. Without `refresh`, MMX restarts. |
| The commit that a `git` node task checked out | What `refresh` says, as for a file. |
| The installed version of a package, or a package removal | A restart, also with `refresh: reload`. |
| A unit file from `systemd_unit` | A restart, also with `refresh: reload`. |

- Several files in one block's `changes` give one reload, as in
  [step 4](#steps).
- A reboot causes no extra reload. MMX's record of what a running service
  loaded starts from the files on disk at the first run after the reboot.
- A service that MMX starts gets one start, and no reload after the start.
  The first run in [Write a playbook](write-a-playbook.md) says
  `service was not running; started` for that reason.
- `refresh: reload` works only on `.service` units, and only when the
  program can reload, as nginx can.
- A failed reload fails the node task. MMX doesn't try a restart instead.
- Some files need a restart and others only a reload, such as an
  application whose new code needs a restart and whose configuration
  needs a reload. Put each kind in its own node task block, and name the
  same service in the `activate` phase of each block.
- MMX tracks only what the block's `changes` phase declares. A file that
  another block, a package, or a person writes is not a tracked input of
  the block.
- A command such as `postmap` can follow a change the same way: a `shell`
  node task in `activate` with `only_when_tracked_inputs_changed: true`.
  [Run a command after a change](../reference/modules/shell.md#run-a-command-after-a-change)
  shows an example.

The [`systemd_service` page](../reference/modules/systemd_service.md#refresh-in-a-node-task-blocks-activate-phase)
lists every rule, and [Service recovery](../reference/shared-module-behavior.md#service-recovery)
says how MMX keeps the record of what each service loaded.

## A run that stopped before the reload

MMX keeps, on the node, a record of the tracked inputs that each service
last loaded. A run decides on a reload by comparing that record with the
files on disk, not by asking whether a file changed in this run. So when a
run wrote a file and then stopped before the reload, the next run still
reloads.

For example, a run changed `shop.conf`, and the node job worker process on
`web01` ended during `validate`, as when the kernel ends a process
because the server ran out of memory. The node job worker process is the
MMX program on the node that does the node's work for one fleet stage:

```text
set_up_nginx / web01: failed
  node job interrupted before recording completion; mmx resume or the next mmx run inspects the actual resource and service state
  ...
  set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, changed
  set_up_nginx / web01 / shop_site/changes/write_site_file: updated "/etc/nginx/conf.d/shop.conf"
    101 bytes, permissions_mode 0644, uid 0, gid 0 -> 121 bytes, permissions_mode 0644, uid 0, gid 0
Run d896a9b4: failed
```

The next `mmx run set_up_web_servers` finds the file as the playbook says,
and still reloads nginx:

```text
  set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, unchanged
  set_up_nginx / web01 / shop_site/validate/check_nginx_configuration: succeeded, unchanged
  set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, changed — tracked input /etc/nginx/conf.d/shop.conf changed after the service last started or reloaded
```

The next run also reloads after a run that failed between the write and
the reload. A restart that `checks_before_restart_or_reload` guards still needs the
approval in the next run. Once a reload succeeded, a failed `verify` does
not repeat the reload in the next run.

Two other ways that a run ends don't leave a node task block halfway:

- **Your computer stops, or `mmx` ends on it.** The node job worker
  process keeps working without the connection, and finishes the node's
  work. `mmx status` then collects the results from the node.
- **You press Ctrl-C, or run `mmx stop`.** Each node finishes its current
  node task block, with `validate`, `activate`, `verify`, `on_failure`, and
  `cleanup`, and then stops.

## The phases in order

A node task block runs its phases on the node in this order. Each phase is
a list of node tasks. Only `changes` is required. What each phase holds is
in the [`task_block` fields](../reference/playbook-file.md#task_block-fields);
this table says what a dryrun does in each.

| Phase | In a dryrun |
| --- | --- |
| `checks_before_restart_or_reload` | Runs, so the dryrun says whether the run may restart. |
| `changes` | Each node task reports what it would change. A `shell` node task runs only its `dryrun` command. |
| `validate` | Runs the `dryrun` command of each `shell` node task on the files as they are now, without the new files. Without a `dryrun` command, or `run_script_in_dryrun: true` for a script that only reads, the node task is skipped. |
| `activate` | Says `would_change` with the reason. Says `skipped` when the service can be checked only after the changes, such as on a new server without nginx. |
| `verify` | Checks the server as it is now. |
| `on_failure` | Never runs. |
| `cleanup` | Runs, with the `dryrun` command of each `shell` node task. |

- What `on_failure` and `cleanup` read, and when they run, is in
  [Put things back when a node task block fails](#put-things-back-when-a-node-task-block-fails).
- A role or another node task block goes in `node_tasks`, not inside a
  phase.
- A dryrun on a new server reports many checks as `not_checked`, because
  an earlier node task, such as `install_nginx`, would change the server
  first. [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
  explains this result.

## Put things back when a node task block fails

`on_failure` holds the node tasks that put things back when the block
failed (the [playbook file reference](../reference/playbook-file.md#put-things-back-when-a-node-task-block-fails)
lists what it may hold and read). This playbook upgrades an application, and when the upgrade fails,
`on_failure` restores the backup and starts the old release again:

```yaml
# playbooks/upgrade_shop.yml
command_line_vars:
  release:
    type: string

fleet_stages:
  - id: upgrade_shop
    nodes: web_servers
    node_tasks:
      - id: create_backup_directory
        directory:
          path: /var/backups/shop
          owner: root
          group: root
          permissions_mode: "0700"

      - id: installed_release
        shell:
          # $(...) drops the line end, so stdout equals "2.4.0".
          script: printf %s "$(/usr/local/bin/shop version)"
          dryrun: printf %s "$(/usr/local/bin/shop version)"
          changed_when: false

      - when:
          not:
            equals: ["{{ .node_tasks.installed_release.stdout }}", "{{ .vars.release }}"]
        task_block:
          id: upgrade
          changes:
            - id: stop_shop
              systemd_service:
                name: shop.service
                state: stopped
            - id: back_up_release_and_data
              shell:
                script: |
                  set -eu
                  rm -rf "$BACKUP"
                  mkdir "$BACKUP"
                  cp -a /usr/local/bin/shop "$BACKUP/shop"
                  tar -C /var/lib/shop -cpf "$BACKUP/data.tar" .
                  touch "$BACKUP/complete"
                env:
                  BACKUP: "/var/backups/shop/before-{{ .vars.release }}"
            - id: install_new_release
              file:
                path: /usr/local/bin/shop
                source: "files/shop-{{ .vars.release }}"
                owner: root
                group: root
                permissions_mode: "0755"
            - id: migrate_data
              shell:
                script: /usr/local/bin/shop migrate
                run_as_user_account: shop
          activate:
            - id: start_shop
              systemd_service:
                name: shop.service
                state: running
          verify:
            - id: check_shop_answers
              wait_for:
                tcp_port_accepts_connections: 8080
                wait_at_most: 30s
          on_failure:
            - id: restore_data
              shell:
                script: |
                  set -eu
                  test -f "$BACKUP/complete"
                  systemctl stop shop.service
                  find /var/lib/shop -mindepth 1 -delete
                  tar -C /var/lib/shop -xpf "$BACKUP/data.tar"
                env:
                  BACKUP: "/var/backups/shop/before-{{ .vars.release }}"
            - id: restore_old_release
              file:
                path: /usr/local/bin/shop
                content_from_node_paths: ["/var/backups/shop/before-{{ .vars.release }}/shop"]
                owner: root
                group: root
                permissions_mode: "0755"
            - id: start_old_release
              systemd_service:
                name: shop.service
                state: running
```

`mmx run upgrade_shop --var release=2.4.0` skips the block on a node that
already runs 2.4.0. On another node, when the migration fails, MMX skips
`activate` and `verify` and runs `on_failure`: the old data, the old
program, and a start of the old release. The backup script writes
`$BACKUP/complete` last, and the restore script deletes the data only when
that file exists, so a failure before the backup finished leaves
`/var/lib/shop` as it is.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `required validation did not pass: exit status 1` and the program's own message, such as `unknown log format "shop_log" in /etc/nginx/conf.d/shop.conf:4` | The new files fail the `validate` check. Fix the file that the message names, in the playbook, and run again. Without `on_failure`, the broken file stays on the server until then. |
| `check_new_file failed: exit status 1`, with `visudo`'s message, such as `/etc/sudoers.d/.mmx-candidate-EQNVLJJK2CBWDENCTB45SVQLEE/shop-deploy:1:59: syntax error` | The check rejected the new file, and the live file stays as it was. The message names the line and the column. Fix the content in the playbook, and run again. |
| `file.check_new_file.shell.dryrun runs as written, so the shell would receive the literal text {{ .new_file.path }}` | A `dryrun` command contains a `{{ ... }}` reference. Pass the value through `env`, as in [step 7](#steps). |
| `memcached must restart to load this change ...` from `require_restart_approval` | Your own `checks_before_restart_or_reload` message. Run again with `--var restart_approved=true` when a restart is acceptable. |
| `put_back_working_files: skipped, unchanged — node task skipped by a command condition` after a failed `verify` | The files on disk pass `nginx -t`, so MMX left the new files in place. nginx already runs the new files. Fix the cause that `verify` names, and run again. |
| `wait_for ended after at most 1m0s: context deadline exceeded; last observation: HTTP status 200; response_body does not contain the required text` | nginx answers, but not with the expected text. Check `root` in the site file, and the home page. |
| After a failed `validate`, `nginx -t` on the server still fails | The broken file did not exist before the change, so the copy has no older version of the file to put back. Fix the file in the playbook and run again. |
| `node job interrupted before recording completion` | The node job worker process on the server ended during the run. Run again: MMX reloads what is still due, as [A run that stopped before the reload](#a-run-that-stopped-before-the-reload) shows. |
| `reload_nginx` says `unchanged` although another file that nginx reads changed, such as `/etc/nginx/nginx.conf` | MMX tracks only the files that the block's `changes` phase writes. Add the file to `changes` if nginx must reload when the file changes. |

## Where to go next

- [Preview and run changes](preview-and-run-changes.md): what each result
  and exit code means.
- [`systemd_service`](../reference/modules/systemd_service.md),
  [`file`](../reference/modules/file.md), and [`shell`](../reference/modules/shell.md): every
  input of the modules on this page. `mmx describe-module NAME` prints a
  module's page.
- An upgrade that stops an application, backs up its data, and restores
  the old release on a failure:
  [Put things back when a node task block fails](#put-things-back-when-a-node-task-block-fails).
- Command-line variables, such as `restart_approved`:
  [Use variables and templates](use-variables-and-templates.md).
- Roles, for reusing a node task block on many kinds of server:
  [Reuse work with roles](reuse-work-with-roles.md).
