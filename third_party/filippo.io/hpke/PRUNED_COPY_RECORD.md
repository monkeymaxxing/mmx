# Pruned copy of filippo.io/hpke

MMX builds with this copy of `filippo.io/hpke` instead of the module from
the Go module proxy: the `replace` line in MMX's `go.mod` points here. The
copy keeps only what MMX uses. [MMX's development
design](../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `filippo.io/hpke`, version `v0.4.0`.
- Module hash: `h1:p575VVQ6ted4pL+it6M00V/f2qTZITO0zgmdKCkd5+A=`,
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

Kept: 9 of 12 Go files, 1,714 of 2,288 lines.

Kept packages:

- `filippo.io/hpke`
- `filippo.io/hpke/crypto`
- `filippo.io/hpke/crypto/ecdh`
- `filippo.io/hpke/internal/byteorder`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `golang.org/x/sys`.

## Applied fixes

None.
