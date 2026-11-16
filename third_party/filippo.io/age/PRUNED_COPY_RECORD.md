# Pruned copy of filippo.io/age

MMX builds with this copy of `filippo.io/age` instead of the module from the
Go module proxy: the `replace` line in MMX's `go.mod` points here. The copy
keeps only what MMX uses. [MMX's development
design](../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `filippo.io/age`, version `v1.3.2`.
- Module hash: `h1:r6RSZLFSMm6rzKepZ7ZAYkKCu14f3/Me8c7uKYh7C8c=`,
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

Kept: 8 of 47 Go files, 2,175 of 11,199 lines.

Kept packages:

- `filippo.io/age`
- `filippo.io/age/internal/bech32`
- `filippo.io/age/internal/format`
- `filippo.io/age/internal/stream`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `c2sp.org/CCTV/age`, `filippo.io/edwards25519`, `filippo.io/nistec`,
  `github.com/rogpeppe/go-internal`, `golang.org/x/sys`,
  `golang.org/x/term`, `golang.org/x/tools`.

## Applied fixes

None.
