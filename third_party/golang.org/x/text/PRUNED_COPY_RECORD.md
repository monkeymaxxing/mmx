# Pruned copy of golang.org/x/text

MMX builds with this copy of `golang.org/x/text` instead of the module from
the Go module proxy: the `replace` line in MMX's `go.mod` points here. The
copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `golang.org/x/text`, version `v0.42.0`.
- Module hash: `h1:JbOZXgfeCPU9gacVtYliJqOhD+zhrEqK4LfdpmlUZqI=`,
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

Kept: 10 of 432 Go files, 11,077 of 593,939 lines.

Kept packages:

- `golang.org/x/text/transform`
- `golang.org/x/text/unicode/norm`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `golang.org/x/mod`, `golang.org/x/sync`, `golang.org/x/tools`.

## Applied fixes

None.
