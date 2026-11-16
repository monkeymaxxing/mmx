# `page_examples`

## Examples

A whole playbook is not a node task list, so the test skips it:

```yaml
# playbooks/site.yml
fleet_stages:
  - id: web
    nodes: all
```

A valid example that uses a variable and a file on the controller machine:

```yaml
- file:
    path: /etc/app.conf
    source: files/app.conf
    permissions_mode: "0644"
- show: "{{ .vars.greeting }}"
```

An example with an input that the module does not have:

```yaml
- file:
    path: /etc/app.conf
    no_such_input: true
```
