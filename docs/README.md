# MMX documentation

Each page answers one question. Start with [Get started](get-started.md)
if you have not run MMX yet.

## Start here

- [Get started](get-started.md): install `mmx`, create a project for one
  server, preview a change, and make it.
- [How MMX works](how-it-works.md): what happens on your computer and on
  each server during a run, the two connections, and what MMX protects.
- [When something fails](when-something-fails.md): an error message or a
  surprise, what it means, and what to do.
- [Examples](../examples/): whole example projects.

## Write and run playbooks

- [Write a playbook](how-to/write-a-playbook.md): install a package, write
  files, and reload a service only when its configuration changed.
- [Preview and run changes](how-to/preview-and-run-changes.md): what a
  dryrun shows, each result and exit code, and running on one server first.
- [Change a configuration and reload its service](how-to/change-a-configuration-and-reload-its-service.md):
  check a new configuration before it goes live, reload only when needed,
  and put the old files back when a change fails.

## Describe and reach your servers

- [Describe your servers](how-to/describe-your-servers.md): nodes, groups,
  variables, and a second inventory for staging.
- [Connect to your servers](how-to/connect-to-your-servers.md): host keys,
  passwords, jump hosts, port 7443, and a replaced server.

## Choose servers and roll out

- [Choose which servers a run touches](how-to/choose-which-servers-a-run-touches.md):
  `--limit`, `except_nodes`, groups that may be empty, and running only
  some fleet stages.
- [Roll out a change gradually](how-to/roll-out-a-change-gradually.md): a
  few servers at a time, what a failed server stops, and one server at a
  time behind a load balancer.

## Values, results, and reuse

- [Use variables and templates](how-to/use-variables-and-templates.md):
  values for each server, facts such as memory and CPU count, template
  files, and values given on the command line.
- [Use a result from one server on another](how-to/use-a-result-from-one-server-on-another.md):
  pass an address, a key, or a list of servers from one fleet stage to the
  next, and save results on your controller machine.
- [Reuse work with roles](how-to/reuse-work-with-roles.md): package node
  tasks with inputs, and one role for several distributions.

## Secrets

- [Keep secrets](how-to/keep-secrets.md): the encrypted vault, unlocking it
  for automation, secrets from environment variables, and a vault for each
  inventory.
- [Secrets reference](reference/secrets.md): which secrets a run needs and
  each server receives, and where secrets can appear.

## Control runs and servers

- [Stop, resume, and read a run](how-to/stop-resume-and-read-a-run.md):
  stop a run, finish it later, read its results, and how long runs are
  kept.
- [Run a command on servers now](how-to/run-a-command-on-servers-now.md):
  a one-off command on some servers, without a playbook.
- [Make a one-time change](how-to/make-a-one-time-change.md): work that
  happens once on each server, such as moving data, and marking it done.
- [Keep MMX off a server](how-to/keep-mmx-off-a-server.md): keep runs off
  while you work on a server, or remove MMX from it.
- [Manage the controller machine itself](how-to/manage-the-controller-machine-itself.md):
  let MMX set up the computer that runs `mmx`, and patch it safely.

## Modules, software, and data

- [Find the module for a job](how-to/find-the-module-for-a-job.md): look
  up modules, and what to do when none fits.
- [Write your own module](how-to/write-your-own-module.md): a module for
  your project, in Starlark or as a program.
- [Write a package module](how-to/write-a-package-module.md): support
  another distribution's package manager.
- [Install languages](how-to/install-languages.md): language runtimes,
  an application's dependencies from its lock file, and version managers.
- [Run containers](how-to/run-containers.md): Docker, Compose projects,
  new image versions, and private registries.
- [Manage a database](how-to/manage-a-database.md): PostgreSQL and
  MariaDB accounts and databases, also on another machine.
- [Module interface reference](reference/module-interface.md): what a
  module's description, Starlark primitives, and program protocol hold.

## Work as a team

- [Move or share a project](how-to/move-or-share-a-project.md): another
  controller machine, a teammate, or CI.

## Change risky settings, move in, and upgrade

- [Change SSH or network settings safely](how-to/change-ssh-or-network-settings-safely.md):
  network files that a server puts back by itself, sshd's port and login
  rules, and how to get back in.
- [Move from Ansible or Salt](how-to/move-from-ansible-or-salt.md): each
  Ansible and Salt idea in MMX, and a playbook converted step by step.
- [Upgrade MMX](how-to/upgrade-mmx.md): a new release on your computer and
  your servers, and going back to an earlier one.

## Look things up

- [Commands](reference/commands.md): every `mmx` command and flag, exit
  codes, output, and the JSON of a run.
- [Project settings](reference/project-settings.md): every field of
  `mmx_project_settings.yml`, and what the `.mmx/` directory holds.
- [Inventory file](reference/inventory-file.md): every field of an
  inventory: nodes, groups, variables, and connection settings.
- [Playbook file](reference/playbook-file.md): every field of a playbook,
  retries and time limits, templates, and node facts.
- [Shared module behavior](reference/shared-module-behavior.md): file modes
  and owners, file previews, package versions, service recovery,
  conditions, and the environment of a command.
- [Module pages](reference/modules/README.md): one page for each module.
  `mmx describe-module NAME` prints the same page.
- [Limits on each distribution](reference/limits-on-each-distribution.md): what is
  limited on each tested distribution, and why.
- [Performance](reference/performance.md): the measurements, including the
  comparison with Ansible.

## Changing MMX itself

- [Changing MMX](changing-mmx/design/README.md): the design, the
  acceptance contracts, testing, and the rules for MMX's own code.
