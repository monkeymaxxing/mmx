# Pruned copy of github.com/klauspost/compress

MMX builds with this copy of `github.com/klauspost/compress` instead of the
module from the Go module proxy: the `replace` line in MMX's `go.mod` points
here. The copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `github.com/klauspost/compress`, version `v1.20.1`.
- Module hash: `h1:T7kKElXUMXrUJ2E9QhQhxFtcK5rPyLdsGZvdbLMPdiQ=`,
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

Kept: 57 of 228 Go files, 18,124 of 84,840 lines.

Kept packages:

- `github.com/klauspost/compress`
- `github.com/klauspost/compress/fse`
- `github.com/klauspost/compress/huff0`
- `github.com/klauspost/compress/internal/cpuinfo`
- `github.com/klauspost/compress/internal/le`
- `github.com/klauspost/compress/zstd`
- `github.com/klauspost/compress/zstd/internal/xxhash`

## Changed files

- `fse/compress.go`, `fse/decompress.go`, `huff0/decompress.go`: the fix
  below.

## Applied fixes

- GO-2026-6865 (GHSA-fpmh-frq8-7rj8), panic on crafted zstd input: the
  three source changes of upstream commit
  `7f5d8528720c895a6106f201719f87eb030a565d`, released in v1.20.2.
  `fse` rejects a normalized count that does not fit in an `int16`;
  `huff0.ReadTable` rejects Huffman weights with an FSE accuracy above 6.
  MMX decompresses files that `download` and `unpack_archive` fetch from a
  web address, so the input can come from outside the user's control.
  `TestDecompressorRefusesCraftedZstdHuffmanWeights` in `internal/assets`
  panics without the fix.
