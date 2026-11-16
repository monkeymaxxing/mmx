package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// RecordLastSuccessfulRun replaces the node's record of a playbook's last
// successful run, last_successful_run/<project_name>/<playbook> under the
// node state directory, with one JSON line. The controller process asks for it
// after a run or resume in which every fleet stage that selected this node
// succeeded. The file is root-owned like all node state, and replaced durably
// and atomically, so a reader sees the earlier record or the new one.
func (s Server) RecordLastSuccessfulRun(request protocol.LastSuccessfulRunRequest) error {
	// A one-time playbook's name starts with its date.
	if !modules.Name.MatchString(request.Project) || !(modules.Name.MatchString(request.Playbook) || config.IsOneTimePlaybookName(request.Playbook)) {
		return fmt.Errorf("invalid project or playbook name")
	}
	if err := s.Initialize(); err != nil {
		return err
	}
	dir := filepath.Join(s.Root, "last_successful_run", request.Project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	line, err := json.Marshal(request.Record)
	if err != nil {
		return err
	}
	return durable.Write(filepath.Join(dir, request.Playbook), append(line, '\n'), 0600)
}
