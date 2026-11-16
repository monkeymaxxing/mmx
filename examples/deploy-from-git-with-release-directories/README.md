# Deploy an application from Git, one directory for each release

Set the server address in `inventories/example.yml` and the application's
repository in `greeter_repository`, then run:

```sh
mmx dryrun deploy --var release=v1
mmx run deploy --var release=v1
mmx run deploy --var release=v1
mmx run deploy --var release=v2
```

The application repository holds a small Python web application, like the
files in `example-application/`: `app.py`, `requirements.txt`, and a
`RELEASE` file. Tag a commit `v1` and a later commit `v2`.

The `greeter` role checks out the tag into `/srv/greeter/releases/<tag>` as
the `greeter` account, installs what `requirements.txt` lists into a virtual
environment inside that release, switches the symlink `/srv/greeter/current`
to the release, and installs `greeter.service`. The service starts, and the
role waits until `greeter.service` answers on port 8080 with the release's
name. Last, the role deletes every older release directory.

The second run with `release=v1` changes nothing. The run with `release=v2`
checks out and installs the new release beside the running one, switches the
symlink, and restarts the service once. If that run stops after the switch
and before the restart, the next run still restarts, because the checkout,
the installed Python packages, and the symlink are tracked inputs of the
node task block.

**Why one directory for each release.** The running release is never changed
in place: the new release, with its own installed packages, is complete
before the symlink switches. Going back to `v1` is another run with
`release=v1`, which checks out `v1` again, since the cleanup deleted `v1`.

For the `git` and `pip_package` fields, see the
[`git` page](../../docs/reference/modules/git.md), the
[`pip_package` page](../../docs/reference/modules/pip_package.md), and
[Install languages and application dependencies](../../docs/how-to/install-languages.md).
