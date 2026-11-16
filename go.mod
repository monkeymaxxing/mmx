module mmx

go 1.27.0

toolchain go1.27.4

require (
	filippo.io/age v1.3.2
	github.com/Masterminds/sprig/v3 v3.3.0
	github.com/klauspost/compress v1.20.1
	github.com/ulikunitz/xz v0.5.17
	go.starlark.net v0.0.0-20260908191801-89a6a09411d5
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	golang.org/x/text v0.42.0
)

require (
	dario.cat/mergo v1.0.1 // indirect
	filippo.io/hpke v0.4.0 // indirect
	github.com/Masterminds/goutils v1.1.1 // indirect
	github.com/Masterminds/semver/v3 v3.3.0 // indirect
	github.com/huandu/xstrings v1.5.0 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/spf13/cast v1.7.0 // indirect
)

// MMX builds with pruned copies of its dependencies. The design
// document docs/changing-mmx/design/development.md, section "Pure-Go implementation
// and dependencies", says why and when a copy may change.
replace (
	dario.cat/mergo => ./third_party/dario.cat/mergo
	filippo.io/age => ./third_party/filippo.io/age
	filippo.io/hpke => ./third_party/filippo.io/hpke
	github.com/Masterminds/goutils => ./third_party/github.com/Masterminds/goutils
	github.com/Masterminds/semver/v3 => ./third_party/github.com/Masterminds/semver/v3
	github.com/Masterminds/sprig/v3 => ./third_party/github.com/Masterminds/sprig/v3
	github.com/huandu/xstrings => ./third_party/github.com/huandu/xstrings
	github.com/klauspost/compress => ./third_party/github.com/klauspost/compress
	github.com/mitchellh/copystructure => ./third_party/github.com/mitchellh/copystructure
	github.com/mitchellh/reflectwalk => ./third_party/github.com/mitchellh/reflectwalk
	github.com/shopspring/decimal => ./third_party/github.com/shopspring/decimal
	github.com/spf13/cast => ./third_party/github.com/spf13/cast
	github.com/ulikunitz/xz => ./third_party/github.com/ulikunitz/xz
	go.starlark.net => ./third_party/go.starlark.net
	go.yaml.in/yaml/v3 => ./third_party/go.yaml.in/yaml/v3
	golang.org/x/crypto => ./third_party/golang.org/x/crypto
	golang.org/x/sys => ./third_party/golang.org/x/sys
	golang.org/x/text => ./third_party/golang.org/x/text
)
