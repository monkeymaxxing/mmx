# Official extra modules

The official extra modules are Starlark scripts that the MMX project writes,
tests, and ships in every release, in this directory beside `mmx` and the node
runner executables. `shipped_with_mmx_version` holds the release's version;
`mmx` refuses to run with official extra modules from another release.
[Modules](../docs/changing-mmx/design/modules.md#official-extra-modules) owns the
rules, and the [release plan](../docs/changing-mmx/design/README.md#release-plan) lists the
modules that the first release adds here.
