# Pruned copy of github.com/huandu/xstrings

MMX builds with this copy of `github.com/huandu/xstrings` instead of the
module from the Go module proxy: the `replace` line in MMX's `go.mod` points
here. The copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `github.com/huandu/xstrings`, version `v1.5.0`.
- Module hash: `h1:2ag3IFq9ZDANvthTwTiqSSZLjDc+BedvHPAp5tJy2TI=`,
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

Kept: 3 of 15 Go files, 470 of 2,409 lines.

Kept packages:

- `github.com/huandu/xstrings`

## Changed files

- `convert.go`: the functions MMX does not reach and the declarations
  nothing uses are cut.
- `count.go`: the functions MMX does not reach and the declarations nothing
  uses are cut.

## Applied fixes

None.
