package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type ifupdownPreviewService struct{ t *testing.T }

func (s ifupdownPreviewService) CheckChangeAllowed(context.Context, string) error { return nil }
func (s ifupdownPreviewService) ApplyChange(context.Context, modules.ChangeWithOwedRestore) protocol.Result {
	s.t.Fatal("dryrun asked for a real file change")
	return protocol.Result{}
}

func TestIfupdownSecretTemplateSuppressesTheFileDifference(t *testing.T) {
	// The dryrun reads the real main file; a host with ifupdown has one.
	if _, err := os.Lstat("/etc/network/interfaces"); err == nil {
		t.Skip("this test needs a host without /etc/network/interfaces")
	}
	directory := t.TempDir()
	for name, script := range map[string]string{
		"ifup":      "#!/bin/sh\n[ \"$1\" = --version ] && echo 'ifup version 0.8.44+deb13u1'\ncat > /dev/null\n",
		"ifdown":    "#!/bin/sh\nexit 91\n",
		"ifquery":   "#!/bin/sh\ncat > /dev/null\n",
		"systemctl": "#!/bin/sh\n[ \"$*\" = 'is-enabled networking.service' ] || exit 92\necho enabled\n",
	} {
		writeProgramFileThroughShellChild(t, filepath.Join(directory, name), script)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	input := &modules.IfupdownInterfacesFile{Name: "/etc/network/interfaces", Template: "secret.tmpl", TemplateText: "auto enp6s0\niface enp6s0 inet static\n  address 10.0.0.5/24\n  pre-up echo {{ .secrets.value }}\n"}
	j := job(config.Task{ID: "network", Module: "ifupdown_interfaces_file", ModuleInputs: input})
	j.Dryrun = true
	j.Scope = map[string]any{"secrets": map[string]any{"value": value.Sensitive("network-secret-sentinel")}}
	e := Engine{OwedRestoresForTask: func(path, module string, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
		if path != "network" || module != "ifupdown_interfaces_file" || !runtime.Dryrun {
			t.Fatal(path, module, runtime)
		}
		return ifupdownPreviewService{t}
	}}
	response := e.Run(context.Background(), j)
	requireStatus(t, response, "succeeded")
	result := response.Results[0]
	if !result.Secret || result.FileChange == nil || result.FileChange.Diff != "" || !strings.Contains(result.FileChange.DiffOmitted, "secrets") {
		t.Fatal(result)
	}
	encoded, err := json.Marshal(response)
	if err != nil || strings.Contains(string(encoded), "network-secret-sentinel") {
		t.Fatal("secret reached the response", err)
	}
}
