# Pruned copy of github.com/Masterminds/sprig/v3

MMX builds with this copy of `github.com/Masterminds/sprig/v3` instead of
the module from the Go module proxy: the `replace` line in MMX's `go.mod`
points here. The copy keeps only what MMX uses. [MMX's development
design](../../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `github.com/Masterminds/sprig/v3`, version `v3.3.0`.
- Module hash: `h1:mQh0Yrg1XPo6vjYXgtf5OtijNAKJRNcTdOOGZe3tPhs=`,
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

Kept: 12 of 32 Go files, 1,834 of 5,357 lines.

Kept packages:

- `github.com/Masterminds/sprig/v3`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `github.com/davecgh/go-spew`, `github.com/google/go-cmp`,
  `github.com/google/uuid`, `github.com/mitchellh/reflectwalk`,
  `github.com/pmezard/go-difflib`, `github.com/stretchr/testify`,
  `gopkg.in/yaml.v2`.
- `crypto.go`: the functions MMX does not reach and the declarations nothing
  uses are cut.
- `date.go`: the functions MMX does not reach and the declarations nothing
  uses are cut.
- `functions.go`: the entries of the 38 functions that MMX withholds are cut
  from the function table `genericMap`, as are the functions MMX does not
  reach and the declarations nothing uses.
- `strings.go`: the functions MMX does not reach and the declarations
  nothing uses are cut.

## Applied fixes

None.
