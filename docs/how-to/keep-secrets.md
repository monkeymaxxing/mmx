# Keep secrets

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. A coding agent cannot
type at a prompt, so a coding agent takes the vault passphrase from a file
or an environment variable, as step 1 of the first route shows.

**When to use this page:** a playbook needs a password, an API token, or a
key, and you want the value out of your plain project files and out of
MMX's output.

**What this page does:** you create the vault, an encrypted file of secrets
in your project, add secrets without showing them, write two secrets into a
file on your servers, and run without a terminal. The other routes change a
secret, find where a secret is used, change the passphrase, take secrets
from environment variables, and keep a separate vault for staging.

**Terms:**

- A **node** is a server that MMX manages. The **inventory**, a file such
  as `inventories/example.yml`, lists the nodes. A **playbook** in
  `playbooks/` lists **fleet stages**; each fleet stage picks nodes, and
  each of those nodes runs the fleet stage's **node tasks**.
- A **secret** is a value that MMX keeps out of its output and out of the
  records it saves of each run, such as a database password.
- A **secret key** is the name of one secret, such as `database_password`.
  A node task reads the secret as `{{ .secrets.database_password }}`.
- The **vault** is the file `secrets_vault.age` in your project directory.
  The vault holds secret keys and their values, encrypted with a
  passphrase in the [age](https://age-encryption.org/) format. MMX has age
  built in, so you install nothing else. You can commit the vault to Git.
- The **vault passphrase** unlocks the vault. MMX reads the passphrase on
  your controller machine and never sends the passphrase to a node.

## Choose your route

| You want to | Route |
| --- | --- |
| Start keeping secrets, and use one in a playbook | [Create the vault and use a secret](#create-the-vault-and-use-a-secret) |
| Run from CI, a script, or a coding agent, with no one to type | [Run without a terminal](#run-without-a-terminal) |
| Add a secret that another program gives you, or change a secret's value | [Add or change a secret without showing it](#add-or-change-a-secret-without-showing-it) |
| Read one secret's value yourself, such as to store it in a password manager | [Read one value once](#read-one-value-once) |
| Check which playbooks read a secret, and which secrets are missing or unused | [Find where a secret is used](#find-where-a-secret-is-used) |
| Change the vault passphrase | [Change the passphrase](#change-the-passphrase) |
| Keep secrets in environment variables, such as in CI, and no vault | [Secrets from environment variables](#secrets-from-environment-variables) |
| Keep staging's secrets apart from production's | [A vault for each inventory](#a-vault-for-each-inventory) |
| Pass a value that one node makes, such as a join token, to other nodes | [A secret that a node makes](#a-secret-that-a-node-makes) |

## Before you start

- You have a project where `mmx run` worked, as in
  [Get started](../get-started.md). The examples use the servers `web01`
  at `203.0.113.11` and `web02` at `203.0.113.12`, in the group
  `web_servers` of `inventories/example.yml`.
- The project directory is a Git repository (`git init`), because
  `mmx secrets diff-keys` compares the vault with the last commit.
- Your team has a password manager, or another safe place for the vault
  passphrase. Nobody can open the vault without the passphrase, and MMX
  cannot recover a lost passphrase.

## Create the vault and use a secret

1. **Save a new passphrase in a file on your controller machine.**

   ```sh
   mkdir -p ~/.config/mmx
   (umask 077 && openssl rand -base64 24 > ~/.config/mmx/shop-vault-passphrase)
   export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/shop-vault-passphrase"
   ```

   `openssl rand` makes a random passphrase, and `umask 077` makes the file
   readable by you only. The file is outside the project, so the file
   never goes into Git. Add the `export` line to your shell's startup file,
   such as `~/.zshrc` or `~/.bashrc`. Then each `mmx` command that needs
   the vault reads the passphrase from the file and asks nothing. Copy the
   passphrase from the file into your password manager.

   You can type the passphrase instead: leave out the file and the
   `export` line. Then each command that opens the vault at a terminal asks
   `Vault passphrase:`, and a command that creates the vault also asks
   `Confirm vault passphrase:`. `mmx secrets set` with `<` or a pipe cannot
   ask, so it needs the file or an environment variable. A coding agent
   needs the file, or the environment variable of
   [Run without a terminal](#run-without-a-terminal).

   **Check:** `ls -l ~/.config/mmx/shop-vault-passphrase` shows
   `-rw-------`, and `echo "$MMX_VAULT_PASSWORD_FILE"` prints the file's
   path.

2. **Create the vault with your first secret.**

   ```sh
   mmx secrets edit
   ```

   MMX opens your editor: the editor that `VISUAL` names, else the one that
   `EDITOR` names, else `vi`. A new vault starts with two comment lines:

   ```yaml
   # Named secret values, one per line, such as:
   # database_password: "the password"
   ```

   Add one line with your database's password, then save and close the
   editor:

   ```yaml
   database_password: "the password of your database"
   ```

   MMX encrypts the text and writes the vault:

   ```text
   Saved encrypted vault: secrets_vault.age
   ```

   [`mmx secrets edit`](../reference/secrets.md#mmx-secrets-edit) says how
   MMX handles the editor's file. A coding agent does not use an editor: the commands in step 3 and in
   [Add or change a secret without showing it](#add-or-change-a-secret-without-showing-it)
   create the vault too.

   **Check:** `mmx secrets list-keys` prints `database_password`, and
   `head -n 1 secrets_vault.age` prints `age-encryption.org/v1`, the first
   line of an encrypted age file.

3. **Add a secret that MMX makes up.** An application's cookie secret, for
   example, must be random and must then stay the same:

   ```sh
   mmx secrets add-random session_cookie_secret --length 48
   ```

   ```text
   Added the key session_cookie_secret to the vault secrets_vault.age, with a random value of 48 letters and digits. MMX does not show the value.
   ```

   Without `--length`, the value has 32 letters and digits. The command
   fails when the vault already has the secret key, so a second run of the
   command never replaces a value by accident.

   **Check:** `mmx secrets list-keys` prints both secret keys:

   ```text
   database_password
   session_cookie_secret
   ```

4. **Use the secrets in a node task.** Create
   `playbooks/write_shop_settings.yml`:

   ```yaml
   # playbooks/write_shop_settings.yml
   fleet_stages:
     - id: write_shop_settings
       nodes: web_servers
       node_tasks:
         - id: create_settings_directory
           directory:
             path: /etc/shop

         - id: write_database_settings
           file:
             path: /etc/shop/database.env
             permissions_mode: "0600"         # only root can read the file
             content: |
               DATABASE_PASSWORD={{ .secrets.database_password }}
               SESSION_COOKIE_SECRET={{ .secrets.session_cookie_secret }}
   ```

   MMX unlocks the vault on your controller machine and sends each node
   only the secrets that the node's node tasks read. The file on the
   server holds the values in plain text, as an application needs them,
   so `permissions_mode: "0600"` lets only root read the file.
   [Files that hold secrets](../reference/modules/file.md#files-that-hold-secrets)
   gives the rule for other modes.

   **Check:** `mmx secrets usage` lists both secret keys with the line
   that reads them, and `(none)` after `Referenced but not defined:`:

   ```text
   database_password       vault secrets_vault.age
     playbooks/write_shop_settings.yml:14   file /etc/shop/database.env (content)

   session_cookie_secret   vault secrets_vault.age
     playbooks/write_shop_settings.yml:14   file /etc/shop/database.env (content)

   Referenced but not defined:   (none)
   Defined but never referenced: (none)
   ```

5. **Preview the change.**

   ```sh
   mmx dryrun write_shop_settings --diff
   echo "exit code $?"
   ```

   `--diff` shows each file's difference, except for a node task whose
   result MMX hides, such as one that writes a secret:

   ```text
     write_shop_settings / web01 / write_database_settings: succeeded, would_change
     write_shop_settings / web01 / write_database_settings: would create "/etc/shop/database.env"
       absent -> 119 bytes, permissions_mode 0600, uid 0, gid 0
       contents suppressed for a task using secrets
   ```

   On a server where MMX has not run before, the output also has the line
   `203.0.113.11: setting up MMX through verified SSH`, because the first
   dryrun installs MMX's node program there.

   **Check:** `write_database_settings` says `would_change` on each server,
   and the exit code is 2, which for a dryrun means "something would
   change".

6. **Make the change.**

   ```sh
   mmx run write_shop_settings
   ```

   **Check:** `write_database_settings` says `created
   "/etc/shop/database.env"` on each server, and the run ends with
   `Run ...: succeeded`. On the server, the file holds both values:

   ```text
   $ ssh root@203.0.113.11 cat /etc/shop/database.env
   DATABASE_PASSWORD=the password of your database
   SESSION_COOKIE_SECRET=4UhcJwUvXy2bZuUT1ulOSdKJVI5zYuFO6kKYnKiURYt6Eh4w
   ```

7. **Commit the vault and the playbook.**

   ```sh
   git add secrets_vault.age playbooks/write_shop_settings.yml
   git commit -m "Keep the shop's secrets in the vault"
   ```

   Teammates get the vault with the project, and the passphrase from your
   password manager.

   **Check:** `mmx secrets diff-keys` compares the vault with the last
   commit, and reports no difference:

   ```text
   Vault: secrets_vault.age, compared with the last commit

   Added:    (none)
   Removed:  (none)
   Changed:  (none)
   ```

Module pages that rely on this section: [`user`](../reference/modules/user.md).

## Run without a terminal

MMX asks for the vault passphrase only at a terminal. A CI job, a script,
or a coding agent gives the passphrase in one of these ways:

| Source | Use it for |
| --- | --- |
| `--vault-password-file PATH` on the command | One command, such as a command for another inventory's vault |
| `MMX_VAULT_PASSWORD_FILE`, the path of a file | Your own computer, or a coding agent on it |
| `MMX_VAULT_PASSWORD`, the passphrase itself | CI, which keeps the passphrase in the CI system's secret storage |

MMX uses the first source in the table that is set. [Unlocking the vault](../reference/secrets.md#unlocking-the-vault)
says what happens when that source fails.

1. **Give the passphrase to the command.** In CI, store the passphrase as
   a secret of the CI system, under the environment variable name
   `MMX_VAULT_PASSWORD`. On your own computer, the
   `MMX_VAULT_PASSWORD_FILE` of
   [the first route](#create-the-vault-and-use-a-secret) works too.

   **Check:** `mmx secrets list-keys --non-interactive` prints the secret
   keys. `--non-interactive` makes MMX fail instead of asking a question.

2. **Run with `--non-interactive`.**

   ```sh
   mmx run write_shop_settings --non-interactive --output=summary
   ```

   `--output=summary` puts the servers with the same result on one line,
   which keeps a CI log short:

   ```text
   write_shop_settings / 2 nodes [web01, web02]: succeeded
     write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: succeeded, unchanged
     write_shop_settings / 2 nodes [web01, web02] / create_settings_directory: directory "/etc/shop"
       checked 1 directories and 0 files; 0 metadata updates
     write_shop_settings / 2 nodes [web01, web02] / write_database_settings: succeeded, unchanged
   Run 86a155d0: succeeded
   ```

   **Check:** the run ends with `Run ...: succeeded` and exit code 0.
   Without a passphrase source, the run stops before any server changes,
   with exit code 4:

   ```text
   mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)
   ```

MMX also asks nothing, and fails in the same way, when the command has no
terminal or when the command has `--json`. [Unlocking the vault](../reference/secrets.md#unlocking-the-vault)
lists which commands open the vault.

## Add or change a secret without showing it

`mmx secrets set` and `mmx secrets add-random` write one secret into the
vault. Neither command prints the value, so a coding agent can store a
value that the agent never sees. Both commands create the vault when the
project has none.

1. **Add a value that you have in a file, or that a program prints.** The
   value comes from standard input:

   ```sh
   mmx secrets set mail_api_token < ~/Downloads/mail-api-token.txt
   ```

   A program's output works through a pipe, such as
   `some_program | mmx secrets set mail_api_token`. At a terminal without
   `<` or a pipe, the command asks `Value of mail_api_token:` and hides
   what you type. [`mmx secrets set`](../reference/secrets.md#mmx-secrets-set)
   lists the rules for the value.

   **Check:** the command prints nothing and exits with code 0, and
   `mmx secrets list-keys` prints `mail_api_token`.

   To put a value inside a secret key, give one more word for each level,
   such as for a role that reads several values from one secret key:

   ```sh
   mmx secrets set mail_secret_files smtp-password < ~/Downloads/smtp-password.txt
   ```

   The vault then holds `smtp-password` below `mail_secret_files`, and
   MMX adds `mail_secret_files` when the vault does not have it. A node
   task reads the value as
   `{{ index .secrets.mail_secret_files "smtp-password" }}`.
   [A name inside a secret key](../reference/secrets.md#a-name-inside-a-secret-key)
   gives the rules.

2. **Change the value of a secret key that the vault has.** Add
   `--replace`:

   ```sh
   mmx secrets set mail_api_token --replace < ~/Downloads/mail-api-token.txt
   mmx secrets add-random session_cookie_secret --replace
   ```

   ```text
   Replaced the value of the key session_cookie_secret in the vault secrets_vault.age with a random value of 32 letters and digits. MMX does not show the value.
   ```

   Without `--replace`, both commands refuse a secret key that the vault
   has, and change nothing. To change a value by hand, use
   `mmx secrets edit`.

   **Check:** `mmx secrets diff-keys` lists the secret keys that you added
   or changed since the last commit:

   ```text
   Vault: secrets_vault.age, compared with the last commit

   Added:    mail_api_token
   Removed:  (none)
   Changed:  session_cookie_secret
   ```

3. **Deliver the new value.**

   ```sh
   mmx run write_shop_settings
   ```

   A new value in the vault changes nothing on a server until a run gives
   the value to the node tasks that read the value.

   **Check:** `write_database_settings` says `updated
   "/etc/shop/database.env"` on each server.

   A password that another program also keeps, such as a database's own
   record of a user's password, must change in that program too. Let the
   same run change both, such as with a
   [`postgresql_role`](../reference/modules/postgresql_role.md) node task that reads
   the same secret.

4. **Commit the vault.**

   ```sh
   git add secrets_vault.age
   git commit -m "Add the mail API token, replace the cookie secret"
   ```

   **Check:** `mmx secrets diff-keys` shows `(none)` on all three lines.

## Read one value once

Sometimes a person needs to read one value, such as a passphrase to store
in a password manager. Run this yourself, in a terminal:

```sh
mmx secrets show database_password
```

MMX prints the value and the line
``Clear your terminal afterwards with `clear`.`` Do so when you have copied
the value. `mmx secrets show` prints nothing through a pipe, into a file,
or through a coding agent, so a value never lands in an agent's
conversation or a log by accident. [`mmx secrets show`](../reference/secrets.md#mmx-secrets-show)
gives the rules.

## Find where a secret is used

`mmx secrets usage` reads every playbook, the roles that the playbooks
call, and the inventory, and lists where each secret key is read. The
command contacts no server and prints no value.

1. **List every secret key.**

   ```sh
   mmx secrets usage
   echo "exit code $?"
   ```

   ```text
   database_password       vault secrets_vault.age
     playbooks/write_shop_settings.yml:14   file /etc/shop/database.env (content)

   mail_api_token          vault secrets_vault.age
     (never referenced)

   session_cookie_secret   vault secrets_vault.age
     playbooks/write_shop_settings.yml:14   file /etc/shop/database.env (content)

   Referenced but not defined:   (none)
   Defined but never referenced: mail_api_token
   exit code 0
   ```

   [`mmx secrets usage`](../reference/secrets.md#mmx-secrets-usage)
   explains each line of the output.

   **Check:** `Referenced but not defined:` says `(none)`, and the exit
   code is 0. A secret key in that list is read by a playbook and is in
   neither the vault nor `secrets_from_env`, so a run of that playbook
   would fail. The command then exits with code 1, so CI can run
   `mmx secrets usage` as a check.

2. **Look up one secret key.**

   ```sh
   mmx secrets usage database_password
   ```

   **Check:** the output has only the lines of `database_password`.

A secret key under `Defined but never referenced:` is in the vault and
nothing reads the key. Remove the key with `mmx secrets edit` when no
playbook needs the key any more.

## Change the passphrase

`mmx secrets passwd` encrypts the vault again with a new passphrase. The
secrets in the vault stay the same.

1. **Save the new passphrase in a second file.**

   ```sh
   (umask 077 && openssl rand -base64 24 > ~/.config/mmx/shop-vault-passphrase.new)
   ```

   **Check:** `ls -l ~/.config/mmx/` shows both files with `-rw-------`.

2. **Change the passphrase.**

   ```sh
   mmx secrets passwd --new-vault-password-file ~/.config/mmx/shop-vault-passphrase.new
   ```

   ```text
   Saved encrypted vault: secrets_vault.age
   ```

   MMX opens the vault with the old passphrase, from
   `MMX_VAULT_PASSWORD_FILE`, and encrypts the vault with the new one. At
   a terminal, you can leave out `--new-vault-password-file`: MMX then
   asks `New vault passphrase:` and `Confirm new vault passphrase:`.

   **Check:** the command prints `Saved encrypted vault:` and exits with
   code 0.

3. **Put the new passphrase where the old one was.**

   ```sh
   mv ~/.config/mmx/shop-vault-passphrase.new ~/.config/mmx/shop-vault-passphrase
   ```

   Put the new passphrase in your password manager and in each CI system
   that has the old one too.

   **Check:** `mmx secrets list-keys` prints the secret keys.

4. **Commit the vault.**

   ```sh
   git add secrets_vault.age
   git commit -m "Change the vault passphrase"
   ```

   **Check:** `mmx secrets diff-keys` shows `(none)` on all three lines.
   Before this commit, `mmx secrets diff-keys` fails, because the
   committed vault needs the old passphrase.

The old passphrase still opens the older copies of the vault that it
encrypted, such as the copies in your Git history. If the old passphrase got out, also give each
secret a new value, as in
[Add or change a secret without showing it](#add-or-change-a-secret-without-showing-it).

## Secrets from environment variables

A secret key can come from an environment variable on the controller
machine instead of the vault. A project whose secret keys all come from
environment variables needs no vault and no passphrase. CI systems often
keep secrets as environment variables.

1. **Name an environment variable for each secret key.** Add
   `secrets_from_env` to `mmx_project_settings.yml`:

   ```yaml
   # mmx_project_settings.yml
   project_name: mmx_example
   default_inventory: example
   secrets_from_env:
     database_password: SHOP_DATABASE_PASSWORD           # secret key: environment variable
     session_cookie_secret: SHOP_SESSION_COOKIE_SECRET
   ```

   The playbook stays the same: `{{ .secrets.database_password }}` now
   reads `SHOP_DATABASE_PASSWORD`. Other secret keys still come from the
   vault. [Where a secret's value comes from](../reference/secrets.md#where-a-secrets-value-comes-from)
   says which source wins.

   **Check:** `mmx secrets usage` names the environment variable on each
   secret key's first line:

   ```text
   database_password       environment variable SHOP_DATABASE_PASSWORD
     playbooks/write_shop_settings.yml:14   file /etc/shop/database.env (content)
   ```

2. **Run with the environment variables set.**

   ```sh
   SHOP_DATABASE_PASSWORD='...' SHOP_SESSION_COOKIE_SECRET='...' mmx run write_shop_settings --non-interactive
   ```

   In CI, the CI system sets the environment variables from its secret
   storage, so the command needs no values in front of `mmx`.

   **Check:** the run ends with `Run ...: succeeded`. With an environment
   variable that is not set or is empty, the run stops before any server
   changes:

   ```text
   mmx: secret database_password requires a nonempty environment variable SHOP_DATABASE_PASSWORD
   ```

Passwords that MMX logs in with, `ssh_password` and `sudo_password`, come
from secrets too.
[Connect to your servers](connect-to-your-servers.md#passwords-for-automation)
shows how to set these passwords from secrets.

## A vault for each inventory

An inventory can name a vault of its own with `vault_file`. Staging then
has its own secrets and its own passphrase, and a person who works on
staging needs no production secret.

1. **Name the vault in the staging inventory.**

   ```yaml
   # inventories/staging.yml
   vault_file: staging_secrets_vault.age   # a path from this inventory's directory

   nodes:
     staging-web01: {address: 203.0.113.41, ssh_user: root}

   groups:
     web_servers:
       nodes: [staging-web01]
   ```

   The path starts at the inventory's directory, so the vault is
   `inventories/staging_secrets_vault.age`. [Which vault file](../reference/secrets.md#which-vault-file)
   lists the other places that choose the vault.

   **Check:** `mmx validate --inventory staging write_shop_settings` prints
   `Valid: mmx_example`.

2. **Save staging's passphrase in a file of its own.**

   ```sh
   (umask 077 && openssl rand -base64 24 > ~/.config/mmx/shop-staging-vault-passphrase)
   ```

   **Check:** `ls -l ~/.config/mmx/shop-staging-vault-passphrase` shows
   `-rw-------`.

3. **Add staging's secrets.** `--inventory staging` selects staging's
   vault, and `--vault-password-file` gives staging's passphrase. The first
   command creates the vault:

   ```sh
   mmx secrets add-random database_password --inventory staging --vault-password-file ~/.config/mmx/shop-staging-vault-passphrase
   mmx secrets add-random session_cookie_secret --inventory staging --vault-password-file ~/.config/mmx/shop-staging-vault-passphrase
   ```

   ```text
   Added the key database_password to the vault inventories/staging_secrets_vault.age, with a random value of 32 letters and digits. MMX does not show the value.
   ```

   **Check:** `mmx secrets usage --inventory staging --vault-password-file ~/.config/mmx/shop-staging-vault-passphrase`
   shows `vault inventories/staging_secrets_vault.age` on each secret key,
   and `(none)` after `Referenced but not defined:`.

4. **Run on staging.**

   ```sh
   mmx run write_shop_settings --inventory staging --vault-password-file ~/.config/mmx/shop-staging-vault-passphrase
   ```

   `--vault-password-file` wins over the production file in
   `MMX_VAULT_PASSWORD_FILE`.

   **Check:** the run ends with `Run ...: succeeded`. Without
   `--vault-password-file`, MMX tries the production passphrase and fails
   with `cannot decrypt vault: wrong passphrase or invalid age file`.

## A secret that a node makes

A secret can start on a node, such as a join token that the first server
of a cluster writes and the other servers need. `content_is_secret: true`
on a [`read_file`](../reference/modules/read_file.md) node task marks the file's
content as a secret, and an output made from the content is a secret too.
In this example, a program on `web01` keeps a token in
`/etc/shop/join_token`, and `web02` needs the token:

```yaml
# playbooks/share_join_token.yml
fleet_stages:
  - id: read_join_token
    nodes: web01
    node_tasks:
      - id: join_token
        read_file:
          path: /etc/shop/join_token
          content_is_secret: true
    outputs:
      join_token: "{{ .node_tasks.join_token.content }}"

  - id: write_join_token
    nodes: web02
    node_tasks:
      - id: write_join_token
        file:
          path: /etc/shop/join_token
          permissions_mode: "0600"
          content: "{{ .outputs.read_join_token.web01.join_token }}"
```

[Use a result from one server on another](use-a-result-from-one-server-on-another.md)
explains outputs in full.

The token goes from `web01` through the memory of `mmx` on your controller
machine to `web02`, and to no other server. `mmx results` shows the output
as `{"join_token":"[redacted]"}`. A `shell` node task with
`output_is_secret: true` marks a command's output as a secret in the same
way.

After an interruption, `mmx resume` reads the file on `web01` again, but
does not run a completed `shell` command again. So keep a value that later
fleet stages need after an interruption in a file on the node, and read
the file with `read_file`, as this example does. Pass the content on whole,
as `"{{ .node_tasks.join_token.content }}"` does: an output with text around
the content blocks its users on `mmx resume`, so put the extra text in the
node task that uses the output instead. The
[secrets reference](../reference/secrets.md#secrets-that-start-on-a-node)
gives the rules of travel, saved records, and `mmx resume`.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)` | MMX could not ask for the passphrase: no terminal, `--non-interactive`, or `--json`. Give a passphrase source, as in [Run without a terminal](#run-without-a-terminal). |
| `cannot decrypt vault: wrong passphrase or invalid age file` | The passphrase does not open this vault. Check which source MMX used: `--vault-password-file` first, then `MMX_VAULT_PASSWORD_FILE`, then `MMX_VAULT_PASSWORD`. For another inventory's vault, give that vault's passphrase. |
| `read vault password file /home/you/.config/mmx/shop-vault-passphrase: open /home/you/.config/mmx/shop-vault-passphrase: no such file or directory` | `MMX_VAULT_PASSWORD_FILE` or `--vault-password-file` names a file that is not there. Fix the path. MMX does not then try `MMX_VAULT_PASSWORD` or ask. |
| `vault is missing required secret mail_token` | A playbook reads a secret key that the vault does not have, often a misspelled name. The run stopped before any server changed, unless the missing key is a node's `ssh_password` or `sudo_password`, which MMX reads when it connects to that node. `mmx secrets usage` lists the key under `Referenced but not defined:` with its file and line. Fix the name, or add the secret. |
| `secret database_password requires a nonempty environment variable SHOP_DATABASE_PASSWORD` | `secrets_from_env` binds the secret key to an environment variable that is not set or is empty. Set the environment variable. |
| `the vault already has the key session_cookie_secret; to give it a new random value, add --replace` | `add-random` or `set` refuses to replace a value by accident. Add `--replace` if you want a new value. |
| `the vault's token holds a value, not names, so smtp-password cannot go inside it. ...` | `mmx secrets set token smtp-password` would put a name inside `token`, but `token` holds a value. To keep the value, choose another secret key. To replace the value with names, run `mmx secrets edit` and remove the value of `token`. |
| `secret key database_password is bound to environment variable SHOP_DATABASE_PASSWORD by secrets_from_env in mmx_project_settings.yml. ...` | A run reads this secret key from the environment variable, so a value in the vault would never be read. Remove the `secrets_from_env` entry, or choose another secret key. |
| `vault must contain one YAML mapping with unique secret names` after `mmx secrets edit` | The text you saved is not a list of `key: value` lines, or has a secret key twice. MMX kept the vault as it was. At a terminal, MMX asks `Edit again? [Y/n]`; answer Enter to fix the text in your editor. Without a terminal, your changes are lost, so run `mmx secrets edit` again. |
| `role input database_password receives a secret; declare secret: true` | A role call passes a secret to a role's input. Add `secret: true` to that input in the role's file, below `type` and `required`. |
| `details suppressed for a task using secrets; root can read them on the node in /run/mmx/private/<job-id>-hidden-details-of-failed-node-tasks.txt for 72 hours, or until the node reboots` | A node task with a hidden result failed, such as one that reads a secret, and MMX hid the reason because the reason may hold the secret. Read the file on that server with `sudo cat`. The [secrets reference](../reference/secrets.md#which-node-tasks-have-a-hidden-result) explains which results MMX hides. |
| `mmx secrets diff-keys` says the last committed version `cannot be opened with the supplied passphrase` | You changed the passphrase, and the committed vault still needs the old one. Commit the vault. |
| `mmx secrets diff-keys` says the repository `has no commit yet` | Commit the vault once. Later, `mmx secrets diff-keys` compares with that commit. |
| `new vault passphrase required; use --new-vault-password-file (prompts are disabled)` | `mmx secrets passwd` could not ask for the new passphrase. Give the new passphrase in a file with `--new-vault-password-file`. |

## Where to go next

- [Secrets reference](../reference/secrets.md): which secrets a run needs
  and each server receives, which results MMX hides, where secrets stay on
  a server, and every rule of the vault and the `mmx secrets` commands.
- [Connect to your servers](connect-to-your-servers.md#passwords-for-automation):
  SSH and `sudo` passwords from secrets.
- [`shell`](../reference/modules/shell.md#secret-output): give a command a secret
  through `env`, and show a command's output when the command prints no
  secret.
- [Choose which servers a run touches](choose-which-servers-a-run-touches.md#use-one-inventory-for-each-environment):
  one inventory for each environment.
- [`examples/secrets/`](../../examples/secrets/): a whole project that
  passes a secret from the vault to a role.
