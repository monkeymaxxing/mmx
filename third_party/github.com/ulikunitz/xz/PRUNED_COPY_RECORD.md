# Pruned copy of github.com/ulikunitz/xz

MMX builds with this copy of `github.com/ulikunitz/xz` instead of the module
from the Go module proxy: the `replace` line in MMX's `go.mod` points here.
The copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `github.com/ulikunitz/xz`, version `v0.5.17`.
- Module hash: `h1:flR0y/x1hgM8EGV1AW3Xll6T413G0glV8UfBwR617V4=`,
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

Kept: 36 of 82 Go files, 6,826 of 29,945 lines.

Kept packages:

- `github.com/ulikunitz/xz`
- `github.com/ulikunitz/xz/internal/hash`
- `github.com/ulikunitz/xz/internal/xlog`
- `github.com/ulikunitz/xz/lzma`

## Changed files

None.

## Applied fixes

None.
