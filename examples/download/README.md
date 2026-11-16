# Install a pinned download

Set the HTTPS URL and its trusted SHA-256 digest in
[playbooks/install.yml](playbooks/install.yml), and your server in
[inventories/example.yml](inventories/example.yml). This example expects a
Linux `sleep` executable and runs the executable as a small service. For
your application, use its release artifact and change the unit's `ExecStart`
accordingly.

```sh
mmx validate
mmx dryrun install
mmx run install
mmx run install
```

`validate` checks the declaration without fetching. `dryrun` and `run` each
reuse a saved release after checking the digest, or fetch and verify the
release once on the controller machine. MMX then delivers the verified bytes
to every selected server through the ordinary asset store. No 16 MiB file
limit applies. An unchanged file keeps its inode and causes no service
restart.

To install another release, change the URL and digest. Candidate validation
must pass before the installed file is replaced. If the run fails after
replacing the executable but before activation, `mmx resume` installs the
saved release and finishes its restart, even when the source server is
offline. An ordinary `mmx run install` reads the current declaration and
reuses a saved release after checking the digest, or fetches the source.

URLs and digests are public values and can use references to variables, role
inputs, loop items, and `.node.name`. Destination paths, ownership, and
validator inputs can use the usual references. HTTPS certificate checks stay
on, redirects must remain HTTPS, and a transfer stops after 30 seconds
without bytes. Delete an installed download with an ordinary `file` task
using `state: absent`.

The [CLI suite](../../tests/integration/downloads.py) supplies a local trusted
TLS source and an actual Linux executable padded to 18 MiB. It runs this playbook
on two disposable nodes and inspects both installed and running executable hashes.
