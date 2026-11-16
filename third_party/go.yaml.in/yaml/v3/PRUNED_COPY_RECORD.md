# Pruned copy of go.yaml.in/yaml/v3

MMX builds with this copy of `go.yaml.in/yaml/v3` instead of the module from
the Go module proxy: the `replace` line in MMX's `go.mod` points here. The
copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `go.yaml.in/yaml/v3`, version `v3.0.5`.
- Module hash: `h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=`,
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

Kept: 13 of 18 Go files, 11,346 of 17,456 lines.

Kept packages:

- `go.yaml.in/yaml/v3`

## Changed files

None.

## Applied fixes

None.
