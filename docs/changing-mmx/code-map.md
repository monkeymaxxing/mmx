# Where the code lives

This page is for readers who want to look at the source. The code follows
the path of a run: **controller process, then connection code, then node job
handling code, then node task execution engine, then modules.**

| Area | Where to start | What the code area does |
|---|---|---|
| Commands and output | [`cmd/mmx/main.go`](../../cmd/mmx/main.go), [`presentation.go`](../../cmd/mmx/presentation.go) | Commands, local inspection, and human and JSON output |
| Reading the project | [`internal/config/load.go`](../../internal/config/load.go), [`inspect.go`](../../internal/config/inspect.go), [`assets.go`](../../internal/config/assets.go) | YAML, roles, node selection, checks, explanation, and saving files with the run |
| Running fleet stages | [`internal/controller/run.go`](../../internal/controller/run.go), [`fleet_stages.go`](../../internal/controller/fleet_stages.go), [`store.go`](../../internal/controller/store.go) | The controller fleet scheduler, outputs between fleet stages, resume, and run history |
| Connections and identity | [`internal/controller/connection.go`](../../internal/controller/connection.go), [`ssh.go`](../../internal/controller/ssh.go), [`internal/endpoint`](../../internal/endpoint), [`internal/access`](../../internal/access) | Setup, HTTPS and SSH, authorization, and SSH keys on the controller machine |
| Reachability confirmation | [`internal/controller/reachability.go`](../../internal/controller/reachability.go), [`control.go`](../../internal/controller/control.go) | New connections for each confirmation phase, node deadlines, cancellation, and collecting restore outcomes after lost contact |
| Node processes | [`cmd/mmx-runner/main.go`](../../cmd/mmx-runner/main.go), [`internal/node/node.go`](../../internal/node/node.go), [`requests.go`](../../internal/node/requests.go) | `mmx-runner` commands, the table of JSON node requests that SSH and HTTPS share, accepting node jobs, the node job worker process, status, and stop |
| Changes with owed restores | [`internal/node/owed_restore.go`](../../internal/node/owed_restore.go), [`change_activation.go`](../../internal/node/change_activation.go), [`owed_restore_service.go`](../../internal/node/owed_restore_service.go) | Durable confirmation decisions, activation, systemd restoration, and projecting restore outcomes into interrupted node job results |
| Running node tasks | [`internal/engine/engine.go`](../../internal/engine/engine.go), [`task_block.go`](../../internal/engine/task_block.go), [`service.go`](../../internal/engine/service.go), [`service_recovery.go`](../../internal/engine/service_recovery.go) | The node task execution engine, node task block phases, and service records |
| Modules | [`internal/modules/registry.go`](../../internal/modules/registry.go), [`internal/modules`](../../internal/modules), [`internal/moduleio`](../../internal/moduleio/protocol.go) | The registry of core modules, each core module in its own file, and running Starlark and executable modules |
| Shared pieces | [`protocol`](../../internal/protocol), [`value`](../../internal/value/value.go), [`facts`](../../internal/facts), [`assets`](../../internal/assets), [`secrets`](../../internal/secrets), [`durable`](../../internal/durable) | Result types and [status words](../../internal/protocol/status.go), `{{ ... }}` values, facts, file transfer, the vault, and safe file writes |

In the controller code and saved history, an **entry** tracks one fleet stage
on one node, including its earlier attempts. A resume creates a new node job
only for an entry that needs another attempt.

For a first reading, start with the [nginx example](../../examples/nginx-role/playbooks/web_server.yml)
and its [role](../../examples/nginx-role/roles/web_server/main.yml). Then follow
`Controller.Execute` through `node.Server.Worker` to `Engine.Run` and
`Engine.taskBlock`, then read the connection code. Some tests explain
behavior well: the [controller tests](../../internal/controller/run_test.go) show
batches, failed nodes, and resume; the
[connection tests](../../internal/controller/connection_test.go) show recovery
after a lost answer; the [service tests](../../internal/engine/services_test.go)
show service activation after failures; and the
[stop tests](../../internal/engine/stop_test.go) show why a node task block
finishes before stopping. The [validation record](validation.md) holds current
platform results.
