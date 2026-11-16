# Keep history for a chosen duration

Set your node in `inventories/example.yml`, then run:

```sh
mmx run configure
mmx status
mmx results --json
```

`run_history_retention: 24h` is the default. Choose another positive
duration such as `7d`, or `forever`. The setting governs history on the
controller machine.

Nodes keep each finished job's result for 72 hours, so a controller that
lost contact can still recover the result with `mmx status` or `mmx resume`.
After 72 hours, ordinary MMX traffic removes the node copy. Queued and
running jobs stay.

`mmx results` reads the controller's copy. Recorded resume uses the
controller's copy and gives unfinished work a new job ID. `mmx results` and
recorded resume continue to work after node expiry while the controller copy
is retained. Controller cleanup keeps the latest normal run for this
project/playbook/inventory selection, even when older than retention. Node
cleanup leaves service hashes and installed runner binaries alone.

The [acceptance suite](../../tests/integration/node_history.py) exercises node
expiry, disconnection, and explicit resume on disposable nodes.
