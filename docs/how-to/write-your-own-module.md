# Write your own module

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** no module does a job that your playbooks need
again and again, and a `shell` node task would need several guards or
could not say correctly whether the command changed the server.
[Find the module for a job](find-the-module-for-a-job.md#when-no-module-fits)
helps you decide.

**What this page does:** you write `env_file_setting`, a module that makes
sure an environment file such as `/etc/shop/shop.env` holds one setting,
such as `SHOP_PORT=8080`, and keeps the file's other lines. You describe
the module's inputs, preview the module with a dryrun, run the module, see
the module say `unchanged` on the next run, and restart a service when the
module changed the file. Then you write the same module as an executable.

The core module [`line`](../reference/modules/line.md) sets one line of a file that
already exists. `env_file_setting` also creates the file and returns the
value that the file held before. The module is small on purpose, so that
the page is about writing a module.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** in `playbooks/`
  lists **fleet stages**. Each fleet stage picks nodes, and each of those
  nodes runs the fleet stage's **node tasks**, in order.
- A **module** is the code that does the work of one node task. A **module
  of the project's own** is a directory in the project's
  `external_modules/`, named after the module.
- A **Starlark script** is the usual kind of module of the project's own:
  `module.star`, in [Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md),
  a small language that reads like Python.
- A **primitive** is a function that MMX gives the script to read or
  change the server, such as `read_file` or `write_file`. A script reads
  and changes the server only through primitives.
- The **module description** is the file `module_description.yml`: the
  module's inputs, outputs, and examples.

| What you need | Where |
| --- | --- |
| A module as a Starlark script, the usual kind for a module of your own | [Steps](#steps) |
| A service that restarts when the module changed its file | [Step 9](#steps) |
| A module in Bash, Go, or another language, for a library that Starlark cannot reach | [Write the module as an executable instead](#write-the-module-as-an-executable-instead) |
| What to test before others use the module | [Test the module](#test-the-module) |
| A package module for a distribution that has none | [A package module for your distribution](write-a-package-module.md) |
| Every primitive, field, and rule | [Module interface reference](../reference/module-interface.md) |

## Before you start

- You have an MMX project, as in [Get started](../get-started.md), with the
  group `web_servers` in `inventories/example.yml`. The examples use the
  servers `web01` at `203.0.113.11` and `web02` at `203.0.113.12`, both
  Debian 13.
- A module runs as root on each server, so write or accept only a module
  that you would trust with root access to your servers.

## Steps

1. **Create the module's directory.**

   ```sh
   mkdir -p external_modules/env_file_setting
   ```

   Reason: the directory's name is the module's name, which node tasks
   write. [The module's name](../reference/module-interface.md#the-modules-name)
   lists the rules, such as lower-case letters, digits, and underscores,
   and a name that no core module, official extra module, or node task
   setting has taken.

   **Check:** `ls external_modules` lists `env_file_setting`. MMX checks
   the name in step 3.

2. **Write the script.** Create
   `external_modules/env_file_setting/module.star`:

   ```python
   # external_modules/env_file_setting/module.star
   # Makes sure an environment file, such as /etc/shop/shop.env, sets one
   # setting to one value, and keeps the file's other lines as they are.

   def run_node_task(inputs):
       path = inputs["path"]
       setting = inputs["setting"]
       value = inputs["value"]

       # Rules that module_description.yml cannot state.
       if not path.startswith("/"):
           return {"status": "invalid_inputs", "reason": "path must be an absolute path, such as /etc/shop/shop.env"}
       if not setting.replace("_", "").isalnum() or not setting.isupper() or setting[0].isdigit():
           return {"status": "invalid_inputs", "reason": "setting must use capital letters, digits, and underscores, such as SHOP_PORT"}
       if "\n" in value:
           return {"status": "invalid_inputs", "reason": "value must be one line"}

       # The directory must exist: an earlier node task creates it.
       directory = path.rsplit("/", 1)[0] or "/"
       if not inspect_path(directory).exists:
           return {"status": "missing_prerequisite",
                   "reason": "the directory " + directory + " does not exist; create it with a directory node task before this one"}

       # Read the file as it is now. A missing file counts as empty.
       lines = []
       found = inspect_path(path)
       if found.error:
           return {"status": "failed", "reason": "looking at " + path + ": " + found.error}
       if found.exists:
           if found.type != "file":
               return {"status": "invalid_inputs", "reason": path + " is a " + found.type + ", not a file"}
           current = read_file(path)
           if current.error:
               return {"status": "failed", "reason": "reading " + path + ": " + current.error}
           lines = current.content.splitlines()

       # Replace the setting's line, or add one at the end.
       previous_value = ""
       wanted = setting + "=" + value
       replaced = False
       new_lines = []
       for line in lines:
           if line.startswith(setting + "="):
               if not replaced:
                   previous_value = line[len(setting) + 1:]
                   new_lines.append(wanted)
                   replaced = True
               # A second line for the same setting is dropped.
           else:
               new_lines.append(line)
       if not replaced:
           new_lines.append(wanted)

       # write_file changes the file only when its content, owner, group, or
       # permissions differ, and changes nothing in a dryrun. The file gets
       # owner root, group root, and permissions 0644 (see the note below).
       written = write_file(path, "\n".join(new_lines) + "\n")
       if written.error:
           return {"status": "failed", "reason": "writing " + path + ": " + written.error}
       return {"outputs": {"previous_value": previous_value}}
   ```

   What the script does, in order:

   - MMX calls `run_node_task(inputs)` on the server, with the node task's
     inputs as a dictionary.
   - The script checks the rules that the description cannot state, and
     returns `invalid_inputs` when an input breaks one. MMX then fails the
     node task without another try, because another try would end the
     same way.
   - A missing directory is `missing_prerequisite`, because an earlier
     node task may still create the directory. A dryrun then reports the
     node task `not_checked`, and a run fails the node task.
   - `inspect_path`, `read_file`, and `write_file` are primitives. Each
     primitive returns its answer with an `error` text, which is empty
     when the call worked. A failed call does not stop the script, so the
     script returns `failed` with a reason.
   - The script returns its output `previous_value` and no change. **MMX
     decides the change:** the node task says `changed` when `write_file`
     changed the file, and `unchanged` when the file already matched. In
     a dryrun, `write_file` changes nothing and says whether the call would change
     the file, so
     the script needs no dryrun code of its own.
   - `write_file` gives the file owner root, group root, and permissions
     0644, also when the file exists with other values. A module that
     must keep an existing file's owner, group, and permissions calls
     `inspect_path(path)` first. When `found.exists`, the module passes
     `found.owner`, `found.group`, and `found.permissions_mode` to
     `write_file` as `owner`, `group`, and `permissions_mode`. A new file
     keeps the defaults.

   **Check:** none yet, because MMX reads a module only together with its
   description. Step 3 checks both files.

3. **Describe the module.** Create
   `external_modules/env_file_setting/module_description.yml`:

   ```yaml
   # external_modules/env_file_setting/module_description.yml
   summary: Makes sure an environment file sets one setting to one value, and keeps the file's other lines.
   inputs:
     path:
       type: string
       required: true
       description: The absolute path of the environment file, such as /etc/shop/shop.env. Its directory must exist; the file is created when it is missing.
     setting:
       type: string
       required: true
       description: The setting's name, in capital letters, digits, and underscores, such as SHOP_PORT.
     value:
       type: string
       required: true
       description: The value, on one line, such as "8080". The file gets the line SHOP_PORT=8080.
   outputs:
     previous_value:
       type: string
       description: The value the file had before the node task, or an empty text when the file had no line for the setting.
   examples:
     - description: Set the shop's port to 8080 in /etc/shop/shop.env.
       config: |
         env_file_setting:
           path: /etc/shop/shop.env
           setting: SHOP_PORT
           value: "8080"
   ```

   Reason: `mmx validate` checks every node task that calls the module
   against this description, before any server is contacted. Each input
   needs `type`, `required`, and `description`.
   [The module description](../reference/module-interface.md#the-module-description)
   lists every field and type.

   **Check:**

   ```sh
   mmx describe-module env_file_setting --reference-only
   ```

   ```text
   env_file_setting: the project's own module, Starlark, in external_modules/env_file_setting/
   Makes sure an environment file sets one setting to one value, and keeps the file's other lines.

   Inputs
     path      string   required; The absolute path of the environment file, such as /etc/shop/shop.env. Its directory must exist; the file is created when it is missing.
     setting   string   required; The setting's name, in capital letters, digits, and underscores, such as SHOP_PORT.
     value     string   required; The value, on one line, such as "8080". The file gets the line SHOP_PORT=8080.

   Outputs
     previous_value   string   The value the file had before the node task, or an empty text when the file had no line for the setting.

   Example: Set the shop's port to 8080 in /etc/shop/shop.env.
     env_file_setting:
       path: /etc/shop/shop.env
       setting: SHOP_PORT
       value: "8080"

   Dryrun: MMX runs the Starlark script during a dryrun; functions that would change the node report the proposed changes without applying changes
   Retry: bounded automatic retries by default; retry: false turns them off
   ```

   If the command prints an error instead, the error names the file and
   the line to fix, such as
   `module_description.yml:5: inputs.path: the required field must be set: ...`.
   MMX also checks the example against the inputs.

4. **Call the module from a playbook.** Create
   `playbooks/configure_shop.yml`:

   ```yaml
   # playbooks/configure_shop.yml
   fleet_stages:
     - id: configure_shop
       nodes: web_servers
       node_tasks:
         - id: create_settings_directory
           directory:
             path: /etc/shop

         - id: set_shop_port
           env_file_setting:            # the module in external_modules/env_file_setting/
             path: /etc/shop/shop.env
             setting: SHOP_PORT
             value: "8080"

         - id: show_previous_port
           show: "SHOP_PORT was {{ .node_tasks.set_shop_port.previous_value }}"
   ```

   A node task calls a module of the project's own exactly as it calls a
   core module: the module's name, with its inputs below. A later node
   task reads the module's output as
   `.node_tasks.set_shop_port.previous_value`. `"8080"` is in quotes
   because the description says `value` is a string.

   **Check:** `mmx validate configure_shop` prints `Valid:` and the
   project's name. A mistake names the line and the input, such as
   `playbooks/configure_shop.yml:14:18 (fleet_stages[0].node_tasks[1].env_file_setting.value): expected a string`
   for `value: 8080` without quotes.

5. **Preview the playbook.**

   ```sh
   mmx dryrun configure_shop
   echo "exit code $?"
   ```

   The first dryrun on a new server reports the module `not_checked`,
   because `/etc/shop` does not exist yet. The lines for `web01`:

   ```text
     configure_shop / web01 / create_settings_directory: succeeded, would_change
     configure_shop / web01 / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 1 entries would change
     configure_shop / web01 / set_shop_port: not_checked, unknown — the directory /etc/shop does not exist; create it with a directory node task before this one. The earlier node task create_settings_directory would change this node first and may provide what is missing; if the run then succeeds, change nothing.
     configure_shop / web01 / show_previous_port: not_checked, unknown — ...
   ...
   exit code 3
   ```

   Reason: the script returned `missing_prerequisite`, and a dryrun
   creates nothing, so the directory stays missing. MMX names the earlier
   node task that would create the directory. `show_previous_port` is
   `not_checked` too, because the node task reads `set_shop_port`'s
   output.

   **Check:** `create_settings_directory` says `would_change`,
   `set_shop_port` says `not_checked` with the reason above, and the exit
   code is 3. Once `/etc/shop` exists, the dryrun checks the module for
   real, as step 8 shows.

6. **Run the playbook.**

   ```sh
   mmx run configure_shop
   ```

   ```text
     configure_shop / web01 / create_settings_directory: succeeded, changed
     configure_shop / web01 / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 1 metadata updates
     configure_shop / web01 / set_shop_port: succeeded, changed
       previous_value: ""
     configure_shop / web01 / show_previous_port: succeeded, unchanged
       "SHOP_PORT was "
   ...
   Run 63887955: succeeded
   ```

   **Check:** `set_shop_port` says `succeeded, changed`, and the run ends
   with `Run ...: succeeded` and exit code 0. On the server,
   `cat /etc/shop/shop.env` prints `SHOP_PORT=8080`, and
   `ls -l /etc/shop/shop.env` shows `-rw-r--r-- 1 root root`.
   `previous_value` is empty, because the file was new.

7. **Run the playbook again.**

   ```sh
   mmx run configure_shop --output=summary
   ```

   ```text
   configure_shop / 2 nodes [web01, web02]: succeeded
     configure_shop / 2 nodes [web01, web02] / create_settings_directory: succeeded, unchanged
     configure_shop / 2 nodes [web01, web02] / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 0 metadata updates
     configure_shop / 2 nodes [web01, web02] / set_shop_port: succeeded, unchanged
       previous_value: "8080"
     configure_shop / 2 nodes [web01, web02] / show_previous_port: succeeded, unchanged
       "SHOP_PORT was 8080"
   ```

   `--output=summary` shows the servers with the same results on one
   line.

   **Check:** `set_shop_port` says `unchanged`, because `write_file` found
   the file as the script wanted the file. If the module says `changed` on
   every run, the script writes something different each time, such as a
   time stamp.

8. **Add a second setting, change the port, and preview the difference.**
   Change `value: "8080"` to `value: "8081"`, and add this node task after
   `set_shop_port`:

   ```yaml
         - id: set_log_level
           env_file_setting:
             path: /etc/shop/shop.env
             setting: SHOP_LOG_LEVEL
             value: info
   ```

   Then preview with each file's difference:

   ```sh
   mmx dryrun configure_shop --diff --output=summary
   ```

   ```text
     configure_shop / 2 nodes [web01, web02] / set_shop_port: succeeded, would_change
       previous_value: "8080"
       --- before
       +++ after
       @@ -1,1 +1,1 @@
       -SHOP_PORT=8080
       +SHOP_PORT=8081
     configure_shop / 2 nodes [web01, web02] / set_log_level: succeeded, would_change
       previous_value: ""
       --- before
       +++ after
       @@ -1,1 +1,2 @@
        SHOP_PORT=8080
       +SHOP_LOG_LEVEL=info
   ```

   MMX built the difference from the script's `write_file` call. In a
   dryrun, `set_log_level` reads the file as the file is now, with
   `SHOP_PORT=8080`, because the dryrun did not write `set_shop_port`'s
   change. For a file that other accounts cannot read, such as mode 0640
   with another owner, `--diff` shows the file's name and no content
   difference; the [reference](../reference/module-interface.md#a-starlark-script)
   says so.

   **Check:** both node tasks say `would_change`, and the exit code is 2.
   Then run `mmx run configure_shop`. On the server,
   `cat /etc/shop/shop.env` prints:

   ```text
   SHOP_PORT=8081
   SHOP_LOG_LEVEL=info
   ```

9. **Restart a service when the module changed the file.** A program
   reads an environment file when the program starts, so a changed
   setting needs a restart. Put the module's node tasks in the `changes`
   of a node task block, and the restart in the block's `activate`. The example
   service `shop.service` is a stand-in that prints its port. The whole
   playbook now reads:

   ```yaml
   # playbooks/configure_shop.yml
   fleet_stages:
     - id: configure_shop
       nodes: web_servers
       node_tasks:
         - id: create_settings_directory
           directory:
             path: /etc/shop

         - task_block:
             id: shop_settings
             changes:
               - id: install_shop_unit
                 systemd_unit:
                   name: shop.service
                   content: |
                     [Unit]
                     Description=Shop stand-in that prints its port

                     [Service]
                     EnvironmentFile=/etc/shop/shop.env
                     ExecStart=/bin/sh -c "echo shop listens on port $SHOP_PORT; exec sleep infinity"

                     [Install]
                     WantedBy=multi-user.target

               - id: set_shop_port
                 env_file_setting:
                   path: /etc/shop/shop.env
                   setting: SHOP_PORT
                   value: "8081"

               - id: set_log_level
                 env_file_setting:
                   path: /etc/shop/shop.env
                   setting: SHOP_LOG_LEVEL
                   value: info
             activate:
               - id: restart_shop
                 systemd_service:
                   name: shop.service
                   start_at_boot: true
                   state: running
                   refresh: restart
   ```

   Run `mmx run configure_shop --output=summary`. On the first run, the
   service is new, so MMX starts the service instead of restarting it:

   ```text
     configure_shop / 2 nodes [web01, web02] / shop_settings/activate/restart_shop: succeeded, changed — service was not running; started
   ```

   Then change `value: "8081"` to `value: "8082"`, and run again:

   ```text
     configure_shop / 2 nodes [web01, web02] / shop_settings/changes/set_shop_port: succeeded, changed
     configure_shop / 2 nodes [web01, web02] / shop_settings/changes/set_log_level: succeeded, unchanged
     configure_shop / 2 nodes [web01, web02] / shop_settings/activate/restart_shop: succeeded, changed — tracked input module env_file_setting changed after the service last started or reloaded
   ```

   Reason: the module's `write_file` call changed the file, so the
   fingerprint of the module's kept files differs from the one the
   service last started with. A later run with nothing new says
   `unchanged` for `restart_shop`. [Tracked input](../reference/module-interface.md#the-node-task-that-calls-the-module)
   and [Reload or restart](change-a-configuration-and-reload-its-service.md#reload-or-restart)
   explain the rules.

   **Check:** on the server, `journalctl -u shop.service -o cat | grep listens`
   prints `shop listens on port 8081`, then `shop listens on port 8082`.

## Write the module as an executable instead

An **executable module** is any program that runs on the server, such as a
Bash script or a Go program. Write one when a Starlark script cannot do
the work, such as with a library that exists only in another language.
MMX cannot see what an executable changes, so the executable must do four
things that MMX does for a script:

- change nothing in a dryrun, which MMX calls `check` mode,
- report its own change: `would_change` in a dryrun, `changed` or
  `unchanged` in a run,
- report the difference that `--diff` shows,
- report a fingerprint, when the module is in a node task block's
  `changes`.

The executable must also find on the server whatever it uses, such as
`bash`, because MMX finds no program for an executable.

These steps replace the script of `env_file_setting` with a Bash
executable. A module's directory holds a script or an executable, not
both, and the playbook stays the same.

1. **Write the executable.** Create
   `external_modules/env_file_setting/module_executable`:

   ```bash
   #!/bin/bash
   # external_modules/env_file_setting/module_executable
   # Makes sure an environment file sets one setting to one value, and keeps
   # the file's other lines as they are.
   set -euo pipefail

   helper=$MMX_MODULE_HELPER    # mmx-runner, which reads and writes the JSON for this script
   request=$("$helper" module-json request --inputs path,setting,value)
   mode=$("$helper" module-json get --type string /mode <<< "$request")
   path=$("$helper" module-json get --type string /inputs/path <<< "$request")
   setting=$("$helper" module-json get --type string /inputs/setting <<< "$request")
   value=$("$helper" module-json get --type string /inputs/value <<< "$request")
   change=unchanged

   fail() {
       "$helper" module-json result --mode "$mode" --status failed --change "$change" --reason "$1"
       exit 1
   }

   # Rules that module_description.yml cannot state. $(...) drops a final
   # newline, so count the lines of value as the helper prints it.
   [[ $path == /* ]] || fail "path must be an absolute path, such as /etc/shop/shop.env"
   [[ $setting =~ ^[A-Z_][A-Z0-9_]*$ ]] || fail "setting must use capital letters, digits, and underscores, such as SHOP_PORT"
   lines=$("$helper" module-json get --type string /inputs/value <<< "$request" | wc -l)
   [[ $lines -eq 0 ]] || fail "value must be one line"

   # The directory must exist: an earlier node task creates it. A dryrun
   # reports the task not_checked, because that node task has not run yet.
   directory=$(dirname -- "$path")
   if [[ ! -d $directory ]]; then
       reason="the directory $directory does not exist; create it with a directory node task before this one"
       if [[ $mode == check ]]; then
           "$helper" module-json result --mode check --status not_checked --change unknown --reason "$reason"
           exit 0
       fi
       fail "$reason"
   fi
   if [[ -L $path || ( -e $path && ! -f $path ) ]]; then
       fail "$path is not a regular file"
   fi

   # Build the new content: replace the setting's line, or add one at the end.
   previous_value=""
   found=no
   new_content=""
   if [[ -f $path ]]; then
       while IFS= read -r line || [[ -n $line ]]; do
           if [[ $line == "$setting="* ]]; then
               if [[ $found == no ]]; then
                   previous_value=${line#"$setting="}
                   new_content+="$setting=$value"$'\n'
                   found=yes
               fi
           else
               new_content+="$line"$'\n'
           fi
       done < "$path"
   fi
   [[ $found == yes ]] || new_content+="$setting=$value"$'\n'
   fingerprint=$(printf '%s' "$new_content" | sha256sum | cut -d ' ' -f 1)

   # Compare with the file on the node: content, owner, group, and permissions.
   old_file=/dev/null
   if [[ -f $path ]]; then
       old_file=$path
   fi
   if [[ $old_file == /dev/null ]] \
       || ! cmp -s -- "$path" <(printf '%s' "$new_content") \
       || [[ $(stat -c '%U %G %a' -- "$path") != "root root 644" ]]; then
       change=would_change
   fi
   difference=""
   if [[ $change == would_change ]]; then
       difference=$(diff -u --label before --label after -- "$old_file" <(printf '%s' "$new_content") || true)
   fi

   # A run writes a new file beside the old one, then renames it into place.
   if [[ $mode == apply && $change == would_change ]]; then
       change=unchanged
       temporary=$(mktemp "$directory/.env_file_setting.XXXXXX") || fail "could not create a file in $directory"
       printf '%s' "$new_content" > "$temporary" || fail "could not write $temporary"
       chown root:root -- "$temporary" && chmod 0644 -- "$temporary" || fail "could not set the owner and permissions of $temporary"
       mv -f -- "$temporary" "$path" || fail "could not replace $path"
       change=changed
   fi

   printf '%s' "$difference" | "$helper" module-json result --mode "$mode" --change "$change" \
       --output previous_value "$previous_value" --fingerprint "$fingerprint" --diff-stdin
   ```

   What the executable does, in order:

   - MMX starts the executable once for each try, as root, and writes one
     JSON request to its standard input, such as
     `{"protocol":1,"mode":"check","inputs":{"path":"/etc/shop/shop.env","setting":"SHOP_PORT","value":"8081"}}`.
     `mode` is `check` in a dryrun and `apply` in a run.
   - `MMX_MODULE_HELPER` names `mmx-runner`, MMX's program on the server,
     whose `module-json` commands read the request and write the result.
     The [executable protocol](../reference/module-interface.md#the-executable-protocol)
     lists them.
   - The executable compares the file with what the file should be, and
     writes the file only in `apply` mode.
   - The executable prints exactly one JSON result on its standard
     output, with `would_change`, `changed`, or `unchanged`, the
     difference, the fingerprint, and the output `previous_value`.

   Then make the file executable and remove the script:

   ```sh
   chmod +x external_modules/env_file_setting/module_executable
   rm external_modules/env_file_setting/module.star
   ```

   **Check:** `mmx validate configure_shop` prints `Valid:`. If it says
   `the module's module_executable must be executable by its owner`, run
   the `chmod` command.

2. **Try the executable by hand.** On a Linux computer with `mmx`
   installed, send the executable a request in `check` mode, which changes
   nothing. `mmx` provides the same `module-json` commands:

   ```sh
   export MMX_MODULE_HELPER=$(command -v mmx)
   mkdir -p /tmp/env-file-test
   printf 'SHOP_PORT=8080\n' > /tmp/env-file-test/shop.env
   echo '{"protocol":1,"mode":"check","inputs":{"path":"/tmp/env-file-test/shop.env","setting":"SHOP_PORT","value":"8081"}}' \
     | external_modules/env_file_setting/module_executable
   echo "exit $?"
   ```

   ```text
   {"protocol":1,"status":"succeeded","change":"would_change","outputs":{"previous_value":"8080"},"diff":"--- before\n+++ after\n@@ -1 +1 @@\n-SHOP_PORT=8080\n+SHOP_PORT=8081","fingerprint":"c5b0503eeda95d30e4b5e4627d0466ff7ffb3163e8bae2bf5567d9c5e46126cc"}
   exit 0
   ```

   The executable uses `stat -c`, which only GNU `stat` on Linux accepts.
   On a Mac, go on to step 3, which tests the executable on the servers.

   **Check:** the result says `would_change` and `previous_value` is
   `8080`. With `"setting":"shop_port"` instead, the result says
   `"status":"failed"` with the reason, and the exit code is 1. Remove
   `/tmp/env-file-test` afterwards.

3. **Check how MMX sees the module.**

   ```sh
   mmx describe-module env_file_setting --reference-only
   ```

   **Check:** the first line says
   `env_file_setting: the project's own module, executable, in external_modules/env_file_setting/`,
   and the `Dryrun:` line says
   `MMX runs the executable in check mode. MMX cannot see what an executable changes, so the module must keep its own dryrun promise`.
   `mmx module-usage` lists the module as `executable (Bash)`.

4. **Preview and run the playbook.** Change `value: "8082"` to
   `value: "8083"`, then:

   ```sh
   mmx dryrun configure_shop --diff --output=summary
   mmx run configure_shop --output=summary
   mmx run configure_shop --output=summary
   ```

   The dryrun shows the executable's difference and the restart that a
   run would make:

   ```text
     configure_shop / 2 nodes [web01, web02] / shop_settings/changes/set_shop_port: succeeded, would_change
       previous_value: "8082"
       --- before
       +++ after
       @@ -1,2 +1,2 @@
       -SHOP_PORT=8082
       +SHOP_PORT=8083
   ...
     configure_shop / 2 nodes [web01, web02] / shop_settings/activate/restart_shop: succeeded, would_change — tracked input module env_file_setting changed after the service last started or reloaded
   ```

   **Check:** the first run says `set_shop_port: succeeded, changed` and
   `restart_shop: succeeded, changed`. The second run says `unchanged`
   for every node task. On the server, `cat /etc/shop/shop.env` prints
   `SHOP_PORT=8083` and `SHOP_LOG_LEVEL=info`.

The first run after you switch a module between a script and an executable
restarts the service once. [Tracked input](../reference/module-interface.md#the-node-task-that-calls-the-module)
says why.

The [executable protocol](../reference/module-interface.md#the-executable-protocol)
lists every request and result field, the `module-json` commands, and the
rules for status, change, and exit code. A compiled program, such as one in
Go, can ship `module_executable_linux_amd64`, `module_executable_linux_arm64`,
or both; a node whose processor has no build fails the node task.

To build the Go example [`examples/external-modules/go/greeting`](../../examples/external-modules/go/greeting/main.go)
for both processors, run these commands from the root of a checkout of
MMX's source code. `CGO_ENABLED=0` makes a program that needs no C
library on the server:

```sh
mkdir -p /tmp/greeting-build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -o /tmp/greeting-build/module_executable_linux_amd64 ./examples/external-modules/go/greeting
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -o /tmp/greeting-build/module_executable_linux_arm64 ./examples/external-modules/go/greeting
```

Copy the two builds and `module_description.yml` into the module's
directory.

## Test the module

Test a module on a test server before others use the module. Check the
server itself, such as with `cat`, not only the module's result:

- A dryrun on a server that already matches says `unchanged`, and a
  dryrun that would change the server changes nothing.
- A run makes the right change and says `changed`. The next run says
  `unchanged`.
- Each input that breaks a rule fails with a reason that says what to
  write instead.
- A dryrun on a new server, where an earlier node task would create what
  the module needs, says `not_checked`, and the run then succeeds.
- For an executable: stop the program after its change and before its
  result, such as with `kill`, then run the playbook again. The next run
  must finish the work and say `changed` or `unchanged` correctly.
- For a module that takes a password: the saved results and the output
  hold no password. [Secrets in a module](../reference/module-interface.md#secrets-in-a-module)
  says how MMX hides an input marked `private_value: true`.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `set_shop_port: failed, unchanged — setting must use capital letters, digits, and underscores, such as SHOP_PORT` | The script returned `invalid_inputs`. Fix the input in the playbook. MMX did not try again. |
| `failed, unchanged — node task did not finish after 3 tries, because each try failed or left retry.until false: the script of the module env_file_setting ended with an error: module/env_file_setting/module.star:7:21: key "settings" not in dict` | The script stopped with an error at line 7, column 21. A module's node task tries again by default, 3 tries in all. Fix the script. A script that does not parse at all (a syntax error) fails at once, without another try. MMX names the file `module/<name>/module.star`, wherever the module's directory is. |
| `the module env_file_setting in external_modules/: the module needs module.star (a Starlark script) or module_executable` | The module's directory holds neither file. Check the file name. |
| `the module env_file_setting in external_modules/: .../module_description.yml:5: inputs.path: the required field must be set: true if every module call must supply the input, false if the input can be omitted` | The description lacks a field. The number after the file name is the line to fix. |
| `unknown input "settings": the inputs of the module env_file_setting are path, setting, value` | The playbook gives an input that the description does not list. Fix the playbook, or add the input to the description. |
| `the input setting is required: The setting's name, ...` | The playbook leaves out an input that the description marks `required: true`. |
| `the module's module_executable must be executable by its owner` | Run `chmod +x external_modules/env_file_setting/module_executable`. |
| `mmx: external_modules/line: line is a core module of MMX; give the module another name` | Another module has the name. Rename the directory and the node tasks that call the module. [The module's name](../reference/module-interface.md#the-modules-name) lists the taken names. |
| The module says `changed` on every run | The script writes something different each time, or an executable reports `changed` without comparing. Compare the file on the server after two runs. |
| The service does not restart after the module changed the file | The module's node task is not in the same node task block's `changes` as the service's `activate` node task. |
| `module env_file_setting is in a node task block's changes phase; the block has service actions or commands that depend on changed inputs, so the module result must include a fingerprint` | The executable reports no `fingerprint`. Add `--fingerprint` to its `module-json result` call. |

## Where to go next

- [Module interface reference](../reference/module-interface.md): every
  primitive and what it returns, the description's fields, the executable
  protocol, secrets, and limits.
- [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
  node task blocks, checks, and putting a file back when a change fails.
- [A package module for your distribution](write-a-package-module.md):
  a package module, built from the official extra module `pacman_package`.
- [Add a core module](../changing-mmx/adding-core-modules.md): for a change
  to MMX itself, not to your project.
- [`examples/external-modules/`](../../examples/external-modules/README.md):
  a `greeting` module as a Starlark script, a Bash executable, and a Go
  executable.
