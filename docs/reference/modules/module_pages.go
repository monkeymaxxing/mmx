// Package modulepages holds the generated pages of the core modules, so that
// mmx describe-module NAME prints the same page that a user reads in
// docs/reference/modules/. Only the controller program mmx imports this package; the
// node program mmx-runner does not carry the pages.
//
// The generator in ./generator writes the pages from each module's page
// source internal/modules/<module>.md and its description in the registry.
// The documentation map (docs/changing-mmx/documentation-map.md, "Module pages") owns the
// rules.
package modulepages

import (
	"embed"
	"strings"
)

//go:generate go run ./generator

// generatedPages are the generated Markdown files of this directory: one
// page for each core module that has a page source, and the index README.md.
//
//go:embed *.md
var generatedPages embed.FS

// CoreModulePage returns the generated page of a core module without its
// first line, the comment that says the file is generated, and whether the
// module has a page.
func CoreModulePage(moduleName string) (string, bool) {
	if moduleName == "" || strings.ContainsAny(moduleName, "/.") {
		return "", false
	}
	page, err := generatedPages.ReadFile(moduleName + ".md")
	if err != nil {
		return "", false
	}
	text := string(page)
	if strings.HasPrefix(text, "<!--") {
		_, text, _ = strings.Cut(text, "\n")
	}
	return text, true
}
