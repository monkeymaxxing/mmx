# Pruned copy of dario.cat/mergo

MMX builds with this copy of `dario.cat/mergo` instead of the module from
the Go module proxy: the `replace` line in MMX's `go.mod` points here. The
copy keeps only what MMX uses. [MMX's development
design](../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `dario.cat/mergo`, version `v1.0.1`.
- Module hash: `h1:Ra4+bf83h2ztPIQYNP99R6m+Y7KfnARDfID+a+vLl4s=`,
  as the Go checksum database records it for this version.

## What was cut

The copy keeps the packages that MMX's programs and tests use. In each
package it keeps the files that a build for Linux or macOS, on AMD64 or
ARM64, compiles and that MMX needs: the files with a function that MMX
reaches, the assembly files and the Go files that declare their functions,
and the files that declare a type, constant, variable, or method that other
kept code uses. `deadcode` from golang.org/x/tools v0.52.0 measured which
functions MMX reaches. Upstream tests, examples, and tools are cut. Every
kept file is identical to upstream, except the files under "Changed files".

In Sprig and the modules that only Sprig uses, the copy also cuts, inside
kept files, the functions that MMX does not reach and the declarations that
nothing uses.

Kept: 2 of 42 Go files, 448 of 3,642 lines.

Kept packages:

- `dario.cat/mergo`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `gopkg.in/yaml.v3`.
- `merge.go`: the functions MMX does not reach and the declarations nothing
  uses are cut.
- `mergo.go`: the functions MMX does not reach and the declarations nothing
  uses are cut.

## Applied fixes

None.
