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

type networkdPreviewService struct{ t *testing.T }

func (s networkdPreviewService) CheckChangeAllowed(context.Context, string) error { return nil }
func (s networkdPreviewService) ApplyChange(context.Context, modules.ChangeWithOwedRestore) protocol.Result {
	s.t.Fatal("dryrun asked for a real file change")
	return protocol.Result{}
}

func TestNetworkdSecretTemplateSuppressesTheFileDifference(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"networkctl", "journalctl", "systemctl"} {
		script := "#!/bin/sh\nexit 91\n"
		if name == "systemctl" {
			script = "#!/bin/sh\n[ \"$*\" = 'is-active systemd-networkd.service' ] || exit 92\necho active\n"
		}
		writeProgramFileThroughShellChild(t, filepath.Join(directory, name), script)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	input := &modules.SystemdNetworkdFile{Name: "99-mmx-secret-unit-test.network", Template: "secret.tmpl", TemplateText: "[Match]\nName=unused\n[Network]\nDescription={{ .secrets.value }}\n"}
	j := job(config.Task{ID: "network", Module: "systemd_networkd_file", ModuleInputs: input})
	j.Dryrun = true
	j.Scope = map[string]any{"secrets": map[string]any{"value": value.Sensitive("network-secret-sentinel")}}
	e := Engine{OwedRestoresForTask: func(path, module string, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
		if path != "network" || module != "systemd_networkd_file" || !runtime.Dryrun {
			t.Fatal(path, module, runtime)
		}
		return networkdPreviewService{t}
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
