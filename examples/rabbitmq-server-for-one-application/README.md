# A RabbitMQ server for one application

RabbitMQ is a message broker: a server that takes messages from programs and
hands them to other programs through queues. This project installs one on a
node and prepares it for one application.

Set the server address in `inventories/example.yml`, give the password of
the application's RabbitMQ user in the environment of the controller
machine, and run:

```sh
export SHOP_RABBITMQ_PASSWORD='the password of the application'
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX installs RabbitMQ: from the distribution's own packages on Debian and
   Ubuntu, and from the RabbitMQ project's two package repositories on the
   other distributions.
2. It starts the server and enables it for the next boot.
3. It enables the plugin `rabbitmq_management`: RabbitMQ's web interface and
   HTTP API, on port 15672.
4. It creates the virtual host `shop`: a separate set of queues and
   exchanges inside the server, for this application alone.
5. MMX creates the user `shop` with the password, and lets the user do
   everything inside the virtual host `shop` and nothing in any other.
6. MMX sets the policy `limit-queue-length`: no queue of the virtual host
   holds more than 100000 messages.

The first dryrun reports the four RabbitMQ tasks as not checked, because
RabbitMQ is not installed yet. The second run changes nothing. A run after
you changed the password in the environment changes only the user's
password.

The last four tasks call official extra modules, which MMX ships beside
`mmx`: [`rabbitmq_plugin`](../../official_extra_modules/rabbitmq_plugin/README.md),
[`rabbitmq_virtual_host`](../../official_extra_modules/rabbitmq_virtual_host/README.md),
[`rabbitmq_user`](../../official_extra_modules/rabbitmq_user/README.md), and
[`rabbitmq_policy`](../../official_extra_modules/rabbitmq_policy/README.md).
Each page lists the module's inputs and what its errors mean.

**The password.** `mmx_project_settings.yml` reads the secret
`shop_rabbitmq_password` from the environment variable
`SHOP_RABBITMQ_PASSWORD`. To keep `shop_rabbitmq_password` in the project's
encrypted vault instead, remove `secrets_from_env` from
`mmx_project_settings.yml`. Store the secret with
`mmx secrets set shop_rabbitmq_password`. The
application's own configuration file reads `shop_rabbitmq_password`. The
password reaches RabbitMQ's program only through its standard input: it is
on no command line and in no log. The password must not start or end with
white space or hold a line break, because RabbitMQ would store another
password then.

**The application connects** to port 5672 of the node, to the virtual host
`shop`, as the user `shop`. The application declares the queues and
exchanges the application uses.

**The user `guest`.** Every new RabbitMQ server has the user `guest` with
the password `guest`, the tag `administrator`, and every permission on the
virtual host `/`. To remove the user `guest`, add:

```yaml
- rabbitmq_user:
    name: guest
    state: absent
```

**The web interface** needs a user with the tag `management` or
`administrator`; the application's user has none. Give an operator a user
of their own with `tags: [administrator]` and `tags_not_listed: remove`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10.

| Distributions | RabbitMQ comes from | Version when tested |
|---|---|---|
| Debian 12 and 13, Ubuntu 24.04 and 26.04 | the distribution's own package | 3.10 to 4.0 |
| Rocky Linux 9, AlmaLinux 10 | the RabbitMQ project's repositories | 4.3 |

**The RabbitMQ project's repositories** serve Erlang, the language that
RabbitMQ is written in, for x86_64 nodes only. They always hold the newest
RabbitMQ, so a new node can get a newer version than an older node has. MMX
fetches each repository's signing key on the controller machine and checks
it against the SHA-256 in the playbook.
