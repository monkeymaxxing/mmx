# Greeting modules

The Starlark, Bash, and Go modules make sure a file on an MMX node contains
`Hello, Barbara!` followed by one newline, and return that text as the
output `greeting`. Each greeting module is a directory to copy into a
project's `external_modules/greeting/`, with the same
[`module_description.yml`](starlark/greeting/module_description.yml):

- [Starlark](starlark/greeting/module.star) is the way to write a module: a
  short script that writes the file through MMX's `write_file` primitive. MMX
  checks the dryrun, shows the difference, and computes the fingerprint
  itself.
- [Bash](bash/greeting/module_executable) is an executable that uses ordinary
  filesystem commands and MMX's JSON helper.
- [Go](go/greeting/main.go) is an executable that uses only the standard
  library; build it as `module_executable_linux_amd64` and
  `module_executable_linux_arm64`.

A node task calls the Starlark, Bash, or Go module with the following
syntax:

```yaml
- id: greet_barbara
  greeting:
    path: /tmp/greeting.txt
    person: Barbara
```

To try the two executables on your controller machine, run these commands
from the root of a checkout of MMX's source code. The commands start no VMs:

```sh
sh scripts/build.sh "$PWD/.module-helper-build"
python3 tests/integration/module_helper.py --local-only --helper "$PWD/.module-helper-build/mmx"
rm -rf "$PWD/.module-helper-build"
```

The [Write your own module](../../docs/how-to/write-your-own-module.md) page walks through the modules
and shows how to send requests to the executables by hand. The [module interface reference](../../docs/reference/module-interface.md)
owns the project layout, module description, JSON interface, helper
reference, execution rules, and limits.
