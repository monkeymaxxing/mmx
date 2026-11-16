package node

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func (s Server) assetDir(id string) string { return filepath.Join(s.Root, "uploads", id) }
func (s Server) assetLock(id string) (*os.File, error) {
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		return nil, err
	}
	return lock(filepath.Join(s.jobDir(id), "assets.lock"))
}

// Upload runs only behind authenticated SSH or HTTPS. Its lock covers both
// publication and admission; admitted jobs' assets cannot be replaced. Like
// every holder, it takes the lock under admission.lock, which expiry relies
// on.
func (s Server) Upload(ctx context.Context, id string, ref assets.Ref, input io.Reader) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid node job ID")
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := s.Initialize(); err != nil {
		return err
	}
	admission, err := waitLock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		return err
	}
	assetLock, err := s.assetLock(id)
	unlock(admission)
	if err != nil {
		return err
	}
	defer unlock(assetLock)
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "request.json")); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot upload assets for an admitted job")
	}
	return assets.Receive(ctx, s.assetDir(id), ref, input)
}
func (s Server) verifyAssets(nodeTasks []config.Task, catalog map[string]modules.Module, id string) error {
	for _, name := range config.ModuleNames(nodeTasks) {
		if _, ok := catalog[name]; !ok {
			return fmt.Errorf("module %s is missing from the job", name)
		}
	}
	for name, module := range catalog {
		if !modules.ModuleName.MatchString(name) {
			return fmt.Errorf("invalid module name")
		}
		if err := module.Validate(true); err != nil {
			return fmt.Errorf("module %s: %w", name, err)
		}
		if !module.IsStarlark() && module.Executable != modules.ModuleExecutableFile && module.Executable != modules.ModuleExecutableFile+"_linux_"+runtime.GOARCH {
			return fmt.Errorf("module %s executable filename does not match the node architecture", name)
		}
	}
	refs, err := config.RequiredAssets(nodeTasks, catalog)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		ref.Path = filepath.Join(s.assetDir(id), ref.SHA256)
		if err := assets.Verify(context.Background(), ref); err != nil {
			return fmt.Errorf("required asset unavailable: %w", err)
		}
	}
	return nil
}

// nodeRetention keeps finished jobs, never-admitted stop markers, and
// unsubmitted uploads (see expireUnadmitted).
const nodeRetention = protocol.NodeJobRecordRetention
