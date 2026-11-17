# Private configuration

Set the SSH destination in `inventories/example.yml`. This example writes a
root-owned configuration file with mode `0600` using one explicit secret
role input. The secret role input also works inside a file-and-node task
block such as the [nginx role](../nginx-role/).

Create `secrets_vault.age` with the secret `database_password`. When
prompting is enabled and no passphrase source is configured, MMX asks for
and confirms the new vault's passphrase, then asks for the secret's value
and hides what you type:

```sh
mmx secrets set database_password
```

MMX encrypts the file using its built-in age library. The encrypted file can
be committed with the project. Set up a private controller password file if
you want subsequent commands to unlock the vault without prompting:

```sh
export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/example-vault-password"
mmx dryrun private_config
mmx run private_config
```

The password file contains the vault passphrase on one line and should have
mode `0600`. For one invocation, use `--vault-password-file PATH`. CI can supply
that file or set `MMX_VAULT_PASSWORD`. Change the vault passphrase with
`mmx secrets passwd`; noninteractive changes take `--new-vault-password-file`.

To supply the database password directly from the environment, add this
binding to `mmx_project_settings.yml`. The role and the role's caller stay
unchanged, and no vault is read:

```yaml
secrets_from_env:
  database_password: MMX_DATABASE_PASSWORD
```

The installed configuration contains the actual password. MMX's history and
diagnostics suppress secret values. A resume refreshes named inputs for
unfinished work, including a password rotated after the failed attempt. A
new node job attempt starts the node tasks from the beginning. Successful
node jobs are not replayed.
