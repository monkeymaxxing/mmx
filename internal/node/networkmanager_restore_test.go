package node

import (
	"context"
	"encoding/json"
	"fmt"
	"mmx/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkManagerNativeActivationFailureSuppressesSecretsInResultAndSavedRecord(t *testing.T) {
	for _, failure := range []string{"load", "activation"} {
		for _, secret := range []bool{false, true} {
			t.Run(failure+fmt.Sprint(secret), func(t *testing.T) {
				f := newRestoreFixture(t)
				// Use the production registry and adapter. Only the native executables
				// are stand-ins; no network or managed file on this machine is changed.
				f.server.activationFactory = nil
				f.destination = "/etc/NetworkManager/system-connections/System secret test.nmconnection"
				r := f.record(t, activatingConfig, protocol.ConnectionSSH)
				r.Outcome.ModuleName, r.Outcome.Secret = "networkmanager_connection_file", secret
				r.ModuleRestoreData = json.RawMessage(`{"proposed_file_present":true}`)
				if err := f.server.writeOwedRestore(&r); err != nil {
					t.Fatal(err)
				}
				directory := t.TempDir()
				const diagnostic = "native-diagnostic-secret-sentinel"
				journal := "#!/bin/sh\necho '-- cursor: before-reload'\n"
				native := "#!/bin/sh\n[ \"$1 $2\" = \"connection load\" ] || exit 90\necho '" + diagnostic + "' >&2\nexit 7\n"
				prefix := "NetworkManager did not accept " + f.destination
				if failure == "activation" {
					prefix = "NetworkManager could not activate the profile of " + f.destination
					journal = "#!/bin/sh\ncase \" $* \" in *' --show-cursor '*) echo '-- cursor: before-reload';; esac\n"
					native = "#!/bin/sh\ncase \"$*\" in\n" +
						"'connection load '*) exit 0;;\n" +
						"'-t --escape no -f UUID,FILENAME connection show') printf '%s\\n' '11111111-1111-1111-1111-111111111111:" + f.destination + "';;\n" +
						"'connection up uuid '*) echo '" + diagnostic + "' >&2; exit 4;;\n" +
						"*) exit 90;; esac\n"
				}
				for name, contents := range map[string]string{"journalctl": journal, "nmcli": native} {
					writeProgramFileThroughShellChild(t, filepath.Join(directory, name), contents)
				}
				t.Setenv("PATH", directory)
				if err := f.server.ActivateChange(context.Background(), r.Outcome.OwedRestoreID); err != nil {
					t.Fatal(err)
				}
				got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
				if got.Phase != restorationRequired || !got.ChangeActivationFailed || got.Outcome.Secret != secret {
					t.Fatal(got)
				}
				result := changeResult(got, protocol.Success("changed"), nil)
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := os.ReadFile(filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "owed_restore.json"))
				if err != nil {
					t.Fatal(err)
				}
				for _, output := range []string{string(encoded), string(saved)} {
					if strings.Contains(output, diagnostic) == secret {
						t.Fatalf("secret=%t: diagnostic visibility is wrong: %s", secret, output)
					}
				}
				if !secret && !strings.Contains(got.Outcome.OriginalFailureReason, prefix) {
					t.Fatal(got)
				}
			})
		}
	}
}
