# Pruned copy of github.com/Masterminds/semver/v3

MMX builds with this copy of `github.com/Masterminds/semver/v3` instead of
the module from the Go module proxy: the `replace` line in MMX's `go.mod`
points here. The copy keeps only what MMX uses. [MMX's development
design](../../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `github.com/Masterminds/semver/v3`, version `v3.3.0`.
- Module hash: `h1:B8LGeaivUe71a5qox1ICM/JLl0NqZSW5CHyL+hmvYS0=`,
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

Kept: 2 of 8 Go files, 1,124 of 3,311 lines.

Kept packages:

- `github.com/Masterminds/semver/v3`

## Changed files

- `constraints.go`: the functions MMX does not reach and the declarations
  nothing uses are cut.
- `version.go`: the functions MMX does not reach and the declarations
  nothing uses are cut.

## Applied fixes

None.
