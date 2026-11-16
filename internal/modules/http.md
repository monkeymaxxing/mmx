# `http`

`http` sends one HTTP or HTTPS request from the node and checks the response
status against `expected_http_status_codes`. It reads a response that a
later node task uses, such as an application's `/version`, or writes to an
application's API with `POST`, `PUT`, `PATCH`, or `DELETE`.

Use [`wait_for`](wait_for.md) when the node needs to wait until something is
ready, and [`assert`](assert.md) to check a value of the response.

## Examples

### Read one response

```yaml
- id: app_version
  http:
    url: http://127.0.0.1:8080/version
    expected_http_status_codes: [200]

- show: "{{ .node_tasks.app_version.response_body }}"
```

The node sends a `GET` to its own port 8080. The task fails unless the
answer has the HTTP status code 200. The next node task shows the response
body.

### Create an account through an application's API only when it is missing

A request that writes is sent once on every run. To make write requests
safe to repeat, ask first and write only when needed. The following two node
tasks create an account through an application's API only when the
application does not have the account:

```yaml
- id: gitea_account
  http:
    url: "http://127.0.0.1:3000/api/v1/users/{{ .vars.gitea_account_name }}"
    expected_http_status_codes: [200, 404]

- when:
    equals: ["{{ .node_tasks.gitea_account.http_status_code }}", 404]
  http:
    method: POST
    url: http://127.0.0.1:3000/api/v1/admin/users
    basic_auth_username: root
    basic_auth_password: "{{ .secrets.gitea_root_password }}"
    json_body:
      username: "{{ .vars.gitea_account_name }}"
      email: "{{ .vars.gitea_account_email }}"
      password: "{{ .secrets.gitea_account_password }}"
      must_change_password: false
    expected_http_status_codes: [201]
```

The first request asks for the account and accepts both 200 (it exists) and
404 (it does not). The second request runs only after a 404. Dryrun sends the
first request and never the second: with the `when` decided by the first
task, it reports the second as `would_change` or skipped exactly, and as not
checked when the first request cannot answer yet.

### Let the API say whether it changed something

`changed_when` works as on [`shell`](shell.md) and reads the response. An API
that says in its answer whether it changed something can decide it:

```yaml
- http:
    method: PUT
    url: http://127.0.0.1:8080/api/settings/mail
    headers:
      Authorization: "Bearer {{ .secrets.app_api_token }}"
    json_body:
      relay_host: mail.example.com
    changed_when:
      equals: ["{{ .this_node_task.response_body_json.changed }}", true]
```

A dryrun cannot know that answer, so it reports such a task as not checked.

## How it works

- **The request.** By default it is a `GET`. Normal TLS checks apply.
  Redirects are not followed, so a 3xx is the HTTP status code. The node task’s `time_limit_per_try`
  and `time_limit` bound the request. `trusted_certificate_authority_file` names a
  certificate on the node to trust for this request, such as an
  application's self-signed one. `dangerously_skip_server_certificate_check: true`
  removes server identity verification; see
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
- **The result.** The result fields are `http_status_code`, `response_body`
  (up to 1 MiB), and `response_body_json` (the body parsed as JSON, read by a
  later node task as data; see
  [conditions](../../docs/reference/shared-module-behavior.md#conditions)). They are separate from the
  node task's own `status`, such as `succeeded` or `failed`. Conditions read
  them through `.this_node_task`, such as `.this_node_task.http_status_code`,
  and later node tasks through `.node_tasks.<id>`.
- **Secrets in a request.** Write a token or a password in `headers`,
  `basic_auth_username`, `basic_auth_password`, `body`, or `json_body` as a
  secret reference, such as `"Bearer {{ .secrets.app_api_token }}"`. A
  secret in one of them hides the result of the task, as for every node task
  that reads a secret. A value written literally in the YAML is no secret:
  it hides nothing, and the response is shown.
- **Changed or unchanged.** A `GET` or `HEAD` reports unchanged by default,
  and a `POST`, `PUT`, `PATCH`, or `DELETE` reports changed.
- **Requests that write.** A `POST`, `PUT`, `PATCH`, or `DELETE` follows the
  contract of a bare `shell` task: MMX sends it once on every run unless a
  condition skips it, and never during a dryrun, which reports it as
  `would_change`. MMX remembers nothing about it on the node.
- **Dryrun.** A `GET` or `HEAD` runs during dryrun, except in a node task
  block with proposed changes, where it is skipped because the new
  configuration is not active yet.
- **One request.** `http` sends one request unless its node task sets
  `retry`; see [node task retries](../../docs/reference/playbook-file.md#node-task-retries-and-time-limits).
- **Checks before a restart.** Only `http` with `GET` or `HEAD` is accepted
  in a node task block's
  [`checks_before_restart_or_reload`](../../docs/reference/playbook-file.md#task_block-fields).

## Errors and what they mean

- **A status code that is not in `expected_http_status_codes`.** The task
  fails. Add the code to the list when the answer is expected, as the first
  example does with 404.
- **`set only json_body or body, not json_body and body together`.** Validation rejects a task with
  `json_body` and `body`. Keep only `json_body` or only `body`.
- **`a request with json_body or body needs method POST, PUT, PATCH, or
  DELETE`.** A `GET` or `HEAD` sends no body. Set `method`.
- **Reading `response_body_json` of an answer that is not JSON.** The
  reading node task fails and says that the output is not valid JSON.
- **A certificate that the node does not trust.** The request fails. Name
  the certificate in `trusted_certificate_authority_file`. A task that
  deliberately removes server identity verification writes
  `dangerously_skip_server_certificate_check: true`.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`http_requests.py`, `downloads.py`, `vm.py test`) and runs on every tested distribution.

The suite `http_requests.py` runs a small account API in Python on the node
and checks what it received: a `POST` sent only after a `GET` found no
account, never during a dryrun; a `PUT` with a secret header whose
`changed_when` reads the answer; a `DELETE`; HTTPS with a self-signed
certificate in `trusted_certificate_authority_file`; and the refusal of a
write in `checks_before_restart_or_reload`. Requests that write were built
and tested on Debian 12 and 13 and Ubuntu 24.04 and 26.04 ([validation
record](../../docs/changing-mmx/validation.md)).

## Why it works this way

- **A request is tried once unless the node task sets `retry`.** Resource
  modules are safe to repeat; a request, like an arbitrary script, may not
  be.
- **The task owns the time limit.** Requests can legitimately take more than
  ten seconds. The ordinary task limits give the playbook author control.
- **A private authority does not require dropping identity checks.** The CA
  file keeps verification; the explicit dangerous setting removes it.
