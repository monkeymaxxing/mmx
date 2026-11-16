# `wait_for`

`wait_for` waits, from the node, until something is ready: a TCP port, a
path, an HTTP response, or a command that only the program itself can
answer. It is the normal node task for a node task block's `verify` phase
after a service is started or reloaded. It changes nothing and reports
unchanged when ready.

Three more forms wait for the health that Docker or Podman reports of a
container or of a Docker Compose project: `docker_container_is_healthy`,
`docker_compose_project_is_ready`, and `podman_container_is_healthy`. The
pages [`docker_compose_project`](docker_compose_project.md) and
[`podman_quadlet`](podman_quadlet.md) describe them.

Use [`http`](http.md) when a later node task reads one response, and
[`assert`](assert.md) to check a value once.

## Examples

### Wait for a port

```yaml
- wait_for:
    tcp_port_accepts_connections: 5432
    wait_at_most: 60s
```

The node checks every second whether something accepts TCP connections on
its own port 5432, for up to 60 seconds.

### Wait for a path

```yaml
- wait_for:
    path_exists: /run/app/ready
    wait_at_most: 30s
```

### Wait for an HTTP response with an expected body

```yaml
- wait_for:
    url_responds: http://127.0.0.1:8080/health
    expected_http_status_codes: [200]
    response_body_contains: ready
    wait_at_most: 30s
```

### Wait for a command

Only the program itself can say that it is ready:

```yaml
- wait_for:
    command_succeeds: kubectl get --raw /readyz
    run_as_user_account: kube
    env:
      KUBECONFIG: /etc/rancher/k3s/k3s.yaml
    wait_at_most: 120s
```

## How it works

Choose exactly one form. Each check runs from the node:

| Check | When ready | Related settings |
|---|---|---|
| `tcp_port_accepts_connections` | A TCP connection succeeds. | `tcp_host` defaults to `127.0.0.1`, the node itself. |
| `tcp_port_refuses_connections` | A TCP connection is refused. A lookup failure or timeout does not count. | Only `tcp_port_accepts_connections` and `tcp_port_refuses_connections` accept `tcp_host`. |
| `path_exists` | The absolute path exists. | |
| `path_is_missing` | The absolute path does not exist. | |
| `url_responds` | A GET returns an accepted HTTP status, and any required body text. | `expected_http_status_codes` defaults to `[200]`; `response_body_contains` is literal text. |
| `command_succeeds` | The command exits with code 0. | Runs as root from `/`; `run_as_user_account`, `run_from_directory`, and `env` work as on [`shell`](shell.md). |

A container form cannot be combined with `tcp_port_accepts_connections`,
`tcp_port_refuses_connections`, `path_exists`, `path_is_missing`,
`url_responds`, or `command_succeeds`. Choose at most one of
`docker_container_is_healthy`, `docker_compose_project_is_ready`, and
`podman_container_is_healthy`.
The name `docker_compose_project_is_ready` includes containers without health
checks: those containers must run or have completed successfully.

- **The wait.** `wait_for` checks every second for up to `wait_at_most`,
  which defaults to `60s` and limits the whole wait. No request can outlast
  the remaining wait.
- **A URL check** sends a `GET`: HTTP or HTTPS, no redirects, a
  10-second request timeout bounded by the remaining wait, and
  a response body of up to 1 MiB. `expected_http_status_codes` and
  `response_body_contains` are allowed only with `url_responds`. The last response
  supplies `http_status_code`, `response_body`, and `response_body_json` in
  the result.
- **HTTPS certificates.** Certificate checks are on by default. For a private
  authority or a self-signed certificate, name the certificate on the node in
  `trusted_certificate_authority_file`. To remove server identity verification,
  explicitly write `dangerously_skip_server_certificate_check: true`; see
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
- **A script check** runs the command with the shell rules. The script must
  only read: MMX runs it again every second, and once during dryrun. When the
  wait ends without readiness, the failure shows the exit code and output of
  the last check, and the result has them as `exit_code`, `stdout`, and
  `stderr`.
- **When time runs out,** `wait_for` fails with the last observation, such
  as the last HTTP status code or connection error: what the last check that
  finished saw. A check that was still running when the wait ended says only
  that it was interrupted, so its interruption is not the observation.
- **Dryrun** checks once without waiting. If the condition is not met,
  `wait_for` reports `not_checked`. Earlier node tasks in a real run may
  make the condition ready.
- **Where `wait_for` is allowed.** In ordinary node task lists and in a
  node task block's `verify`, `activate`, `checks_before_restart_or_reload`,
  `on_failure`, and `cleanup`.
- **Retries and time limits.** Like [`reboot`](reboot.md), `wait_for`
  rejects `retry`, `time_limit`, and `time_limit_per_try` at the node task
  level, ignores `node_task_defaults`, and uses its own `wait_at_most` as the
  only wait limit.

## Errors and what they mean

- **The wait ends without readiness.** The task fails with the last
  observation. For a script, the failure holds the last exit code and
  output; for a URL, the last HTTP status code or connection error. Check
  the service on the node, or give a slow start a longer `wait_at_most`.
- **`retry`, `time_limit`, or `time_limit_per_try` on a `wait_for` task.**
  Validation rejects node task settings `retry`, `time_limit`, and
  `time_limit_per_try`. Use `wait_at_most`.
- **More than one readiness form, or `tcp_host` without a TCP form.**
  Validation rejects the task. Choose exactly one readiness form, from the
  table or the container forms above, and use only the form's related
  settings.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`vm.py test`, `http_requests.py`, `account_units.py`) and runs on every tested distribution.

The suite `account_units.py` covers `wait_for` with `command_succeeds`. Many other
suites wait with `wait_for` for the service they start, and the whole
catalog runs them on Debian 12 and 13 and Ubuntu 24.04 and 26.04 ([validation
record](../../docs/changing-mmx/validation.md)).

## Why it works this way

- **Waiting is a module, not a shell loop.** Keeping waiting in `wait_for`
  makes the readiness condition and its limit visible without a shell
  script.
- **A dryrun reports `not_checked` instead of failing.** Earlier node tasks
  in a real run may make the condition ready.
- **Each form names the whole condition.** A reader can see whether a port
  must accept or refuse connections, or a path must exist or disappear,
  without interpreting a separate state. Compose readiness does not require
  every container to declare a health check.
- **Certificate checks stay on by default.** A private CA file keeps server
  identity verification; the explicit dangerous choice removes that check.
