// greeting is a standalone module: build this file with CGO_ENABLED=0.
// It uses only the Go standard library, not an MMX SDK.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type request struct {
	Protocol int    `json:"protocol"`
	Mode     string `json:"mode"`
	Inputs   struct {
		Path   *string `json:"path"`
		Person *string `json:"person"`
	} `json:"inputs"`
}

type result struct {
	Protocol int               `json:"protocol"`
	Status   string            `json:"status"`
	Change   string            `json:"change"`
	Reason   string            `json:"reason,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
}

func main() {
	r := execute(os.Stdin)
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		os.Exit(1)
	}
	if r.Status == "failed" {
		os.Exit(1)
	}
}

func execute(input io.Reader) result {
	r := result{Protocol: 1, Status: "failed", Change: "unchanged"}
	fail := func(reason string) result {
		r.Status, r.Reason = "failed", reason
		return r
	}
	const limit = 16 << 20
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil || len(data) > limit || !utf8.Valid(data) {
		return fail("request must be UTF-8 JSON within 16 MiB")
	}
	var req request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fail("invalid request or input types")
	}
	if decoder.Decode(new(any)) != io.EOF || req.Protocol != 1 ||
		(req.Mode != "check" && req.Mode != "apply") {
		return fail("expected one protocol 1 request with check or apply mode")
	}
	if req.Inputs.Path == nil || req.Inputs.Person == nil {
		return fail("path and person must be strings")
	}
	path, person := *req.Inputs.Path, *req.Inputs.Person
	if strings.ContainsRune(path+person, 0) {
		return fail("path and person must not contain NUL")
	}
	if strings.ContainsRune(path+person, '\n') {
		return fail("path and person must not contain newlines")
	}
	if !filepath.IsAbs(path) {
		return fail("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err == nil && !info.Mode().IsRegular() {
		return fail("path must be a regular file, not a symlink")
	}
	if err != nil && !os.IsNotExist(err) {
		return fail("could not inspect greeting file")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return fail("parent directory must already exist")
	}
	greeting := "Hello, " + person + "!\n"
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fail("could not read greeting file")
	}
	if err != nil || string(current) != greeting {
		if req.Mode == "check" {
			r.Change = "would_change"
		} else {
			if err := install(path, greeting); err != nil {
				return fail("could not install greeting file")
			}
			r.Change = "changed"
		}
	}
	r.Status, r.Outputs = "succeeded", map[string]string{"greeting": greeting}
	return r
}

func install(path, text string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".mmx-greeting.")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := fmt.Fprint(file, text)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(file.Name(), 0644); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
