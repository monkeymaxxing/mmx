# Pruned copy of golang.org/x/crypto

MMX builds with this copy of `golang.org/x/crypto` instead of the module
from the Go module proxy: the `replace` line in MMX's `go.mod` points here.
The copy keeps only what MMX uses. [MMX's development
design](../../../../docs/changing-mmx/design/development.md#pure-go-implementation-and-dependencies)
gives the rule for changing it.

## Upstream

- Module: `golang.org/x/crypto`, version `v0.57.0`.
- Module hash: `h1:3ZVCjf8Ggz7zneR/EHRVx68Ctf+2pmIMP2UFhh9cC6M=`,
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

Kept: 51 of 303 Go files, 18,302 of 89,922 lines.

Kept packages:

- `golang.org/x/crypto/blowfish`
- `golang.org/x/crypto/chacha20`
- `golang.org/x/crypto/chacha20poly1305`
- `golang.org/x/crypto/cryptobyte`
- `golang.org/x/crypto/cryptobyte/asn1`
- `golang.org/x/crypto/curve25519`
- `golang.org/x/crypto/hkdf`
- `golang.org/x/crypto/internal/alias`
- `golang.org/x/crypto/internal/poly1305`
- `golang.org/x/crypto/pbkdf2`
- `golang.org/x/crypto/scrypt`
- `golang.org/x/crypto/ssh`
- `golang.org/x/crypto/ssh/agent`
- `golang.org/x/crypto/ssh/internal/bcrypt_pbkdf`

## Changed files

- `go.mod`: the requirements that no kept package needs are cut:
  `golang.org/x/net`, `golang.org/x/term`, `golang.org/x/text`.

## Applied fixes

None.
