# Rolling deployment

This example updates two applications behind a load balancer, one server at a
time. The [playbook](playbooks/deploy.yml) contains the whole order:

1. Apply the database migration on `app01`.
2. For each app server in the inventory group `app_servers`, inspect the
   current app and its load-balancer membership.
3. Drain the current app server if an update is needed.
4. Write the current app server's configuration, reload, and check health.
5. Verify the desired release and rejoin the load balancer.
6. Continue with the next app server.

Steps 2 to 5 are one `for_each_node` loop: the controller process runs its
fleet stages in order for `app01`, then for `app02`. The fleet stages on
`loadbalancer` do work for the app server of the current iteration, so
`loadbalancer` is a **helper node** of the loop.

The loop's `on_node_failure: stop_run` decides what a failure stops. A
failure ends the current app server's iteration, and the next app remains
untouched. If the app was successfully drained before the failure, the
failed app stays out of traffic. The `when` on `drain`, `update`, and
`rejoin` is checked by the controller process for each node just before it
would start, so an unchanged healthy run skips `drain`, `update`, and
`rejoin` and the load balancer is not touched. If an earlier run updated an
app but failed before rejoining it, a fresh run sees that the app is current
and drained and finishes the rejoin.

`mmx run deploy --limit app02` runs only `app02`'s iteration, including its
fleet stages on `loadbalancer`, which `nodes`, `explain`, `dryrun`, and `run`
list first under "Nodes outside --limit that help selected nodes".

`mmx resume` uses the saved playbook and selection. `mmx resume` keeps each
node's completed fleet stages and the stages' outputs, then repeats that
node's unfinished fleet stages. `mmx run deploy` reads current files and
makes fresh observations. The migration and membership commands check actual
state when repeated; MMX adds no custom-script recovery ledger.

The inspection role publishes one boolean through its declared `outputs`.
The controller process reads the inspection role's boolean as
`.outputs.inspect_app[.app].update_needed` inside the current iteration.
Each iteration publishes its verified release, keyed by the app server's
node ID; the final fleet stage displays `.outputs.rollout`.

The update role loops over two configuration files inside one node task
block. The two files contribute to the service hash, so they need one reload
together. A failure after writing but before reload is repaired on resume
even when the files already match. A failed health check after a successful
reload does not cause another reload.

## Run the executable example

From the MMX repository:

```sh
python3 tests/integration/incus.py --distribution debian13 --suite deployment.py
```

The Incus test harness creates the disposable Debian test machines and sets
`MMX_TEST_TARGETS` for the suite. The suite prepares the example on the
`mmx-dev` and `mmx-peer` containers. `app01` runs on `mmx-dev`. `app02` and
`loadbalancer` share `mmx-peer`. Test-owned SSH forwards connect the isolated
container networks of `mmx-dev` and `mmx-peer`.
The suite writes its inventory and run records under `deployment/` in the
harness's test slot directory. The harness keeps test slots below
`~/.cache/mmx-testing` by default.

[demo.py](files/demo.py) supplies a small HTTP app that loads configuration on
reload, an HTTP load balancer that routes only to joined backends, a SQLite
schema migration, and inspection/membership commands. The suite installs these
fixtures and their systemd units before invoking MMX, then checks HTTP responses,
actual loaded configuration, reload counts, membership, and migration state.

The `fail-*` files and bounded `hold-drain-app01` gate are test injection
points. The test injection points exercise failures before reload, unhealthy
updated apps, failed rejoins, and controller disconnection after an admitted
drain. The suite also exercises passing a complete backend collection
through a role into a Go template.
