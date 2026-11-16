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

type networkManagerPreviewService struct{ t *testing.T }

func (s networkManagerPreviewService) CheckChangeAllowed(context.Context, string) error { return nil }
func (s networkManagerPreviewService) ApplyChange(context.Context, modules.ChangeWithOwedRestore) protocol.Result {
	s.t.Fatal("dryrun asked for a real file change")
	return protocol.Result{}
}

func TestNetworkManagerSecretTemplateSuppressesTheFileDifference(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"nmcli", "journalctl", "systemctl"} {
		script := "#!/bin/sh\nexit 91\n"
		if name == "nmcli" {
			script = "#!/bin/sh\ncase \"$*\" in '--offline connection modify connection.id '*) cat;; '-t --escape no -f UUID,FILENAME connection show'|'-t --escape no -f GENERAL.DEVICE,GENERAL.STATE,GENERAL.CON-UUID device show') :;; *) exit 93;; esac\n"
		}
		if name == "systemctl" {
			script = "#!/bin/sh\n[ \"$*\" = 'is-active NetworkManager.service' ] || exit 92\necho active\n"
		}
		writeProgramFileThroughShellChild(t, filepath.Join(directory, name), script)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	input := &modules.NetworkManagerConnectionFile{Name: "99-mmx-secret-unit-test.nmconnection", Template: "secret.tmpl", TemplateText: "[connection]\nid={{ .secrets.value }}\ntype=ethernet\ninterface-name=unused\n"}
	j := job(config.Task{ID: "network", Module: "networkmanager_connection_file", ModuleInputs: input})
	j.Dryrun = true
	j.Scope = map[string]any{"secrets": map[string]any{"value": value.Sensitive("network-secret-sentinel")}}
	e := Engine{OwedRestoresForTask: func(path, module string, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
		if path != "network" || module != "networkmanager_connection_file" || !runtime.Dryrun {
			t.Fatal(path, module, runtime)
		}
		return networkManagerPreviewService{t}
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
