package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"mmx/internal/assets"
	"mmx/internal/modules"
)

// Phase a private working directory once per module in this job. Copying leaves
// content-addressed inputs unchanged if module code modifies its own files.
func stageModules(ctx context.Context, catalog map[string]modules.Module, assetsDir string) error {
	if len(catalog) == 0 {
		return nil
	}
	root, err := os.MkdirTemp(assetsDir, "modules-")
	if err != nil {
		return err
	}
	for name, module := range catalog {
		if !modules.Name.MatchString(name) {
			return fmt.Errorf("invalid module name")
		}
		if err := module.Validate(true); err != nil {
			return err
		}
		module.Directory = filepath.Join(root, name)
		for _, file := range module.Files {
			target := filepath.Join(module.Directory, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := stageModuleFile(ctx, file, target); err != nil {
				return err
			}
		}
		catalog[name] = module
	}
	return nil
}

func stageModuleFile(ctx context.Context, file modules.ModuleFile, target string) error {
	source, err := assets.Open(file.Asset.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	if file.Mode&0111 != 0 {
		// A different goroutine can fork while a Go write holds this program
		// open. Its child inherits the writable descriptor until exec, which
		// can make this module's execution fail with "text file busy".
		// Only the shell child opens the executable for writing.
		write := exec.CommandContext(ctx, "/bin/sh", "-c", `umask 077 && set -C && cat > "$1" && chmod "$2" "$1"`, "sh", target, fmt.Sprintf("%o", file.Mode))
		write.Stdin = source
		if output, err := write.CombinedOutput(); err != nil {
			return fmt.Errorf("writing module program %s: %w: %s", target, err, output)
		}
		return nil
	}
	destination, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer destination.Close()
	if _, err := assets.Copy(ctx, destination, source); err != nil {
		return err
	}
	if err := destination.Chmod(os.FileMode(file.Mode)); err != nil {
		return err
	}
	return destination.Close()
}
