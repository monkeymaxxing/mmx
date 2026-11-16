# Pruned copy of golang.org/x/sys

MMX builds with this copy of `golang.org/x/sys` instead of the module from
the Go module proxy: the `replace` line in MMX's `go.mod` points here. The
copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `golang.org/x/sys`, version `v0.48.0`.
- Module hash: `h1:bbX/i/6MgT9BVLM9RT1thmxL04yeTAhbEz4SyadbXoo=`,
  as the Go checksum database records it for this version.

## What was cut

The copy keeps the packages that MMX's programs and tests use. In each
package it keeps the files that a build for Linux or macOS, on AMD64 or
ARM64, with or without the race detector, compiles and that MMX needs: the
files with a function that MMX reaches, the assembly files and the Go files
that declare their functions, and the files that declare a type, constant,
variable, or method that other kept code uses. `deadcode` from
golang.org/x/tools v0.52.0 measured which functions MMX reaches. Upstream
tests, examples, and tools are cut. Every kept file is identical to
upstream, except the files under "Changed files".

Kept: 61 of 487 Go files, 37,516 of 253,423 lines.

Kept packages:

- `golang.org/x/sys/cpu`
- `golang.org/x/sys/unix`

## Changed files

None.

## Applied fixes

None.
