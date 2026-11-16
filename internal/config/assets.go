package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/modules"
)

// assetSource is a local file, template, or download: a url with its sha256,
// or with none where the task says dangerously_skip_checksum. A download with
// decompress is saved decompressed; its sha256 is the file as published.
type assetSource struct {
	path        string
	template    *string
	url, sha256 string
	decompress  string
}

// fileAssets visits all static task branches, including cleanup and role calls.
// Loop iterations refer to the same saved file.
func fileAssets(nodeTasks []Task, visit func(*assets.Ref, assetSource) error) error {
	for i := range nodeTasks {
		t := &nodeTasks[i]
		// A variable source field has one saved file for each value it
		// resolves to, and none of its own.
		for _, s := range taskSources(t) {
			if s.Dynamic() || len(*s.Variants) > 0 {
				if s.Kind == modules.SourceKeys {
					continue // key lines travel in the task; there is no file to save
				}
				for _, key := range sortedKeys(*s.Variants) {
					variant := (*s.Variants)[key]
					if s.Kind == modules.SourceDirectory {
						for i := range variant.Entries {
							if entry := &variant.Entries[i]; !entry.Directory {
								if err := visit(entry.Asset, assetSource{path: entry.LocalPath}); err != nil {
									return err
								}
							}
						}
						continue
					}
					source := assetSource{path: variant.LocalPath, url: variant.URL, sha256: variant.SHA256, decompress: s.Decompress}
					if s.Kind == modules.SourceTemplate {
						source.template = &variant.TemplateText
					}
					if err := visitSource(&variant.Asset, source, visit); err != nil {
						return err
					}
				}
				continue
			}
			switch s.Kind {
			case modules.SourceKeys:
				// key lines travel in the task; there is no file to save
			case modules.SourceDirectory:
				if err := s.CheckTree(); err != nil {
					return err
				}
				for i := range *s.Entries {
					entry := &(*s.Entries)[i]
					if !entry.Directory {
						if err := visit(entry.Asset, assetSource{path: entry.LocalPath}); err != nil {
							return err
						}
					}
				}
			default:
				source := assetSource{path: *s.Local}
				if s.Kind == modules.SourceDownload {
					source.url, source.sha256, source.decompress = *s.URL, *s.SHA256, s.Decompress
				}
				if s.Kind == modules.SourceTemplate {
					source.template = s.Template
				}
				if err := visitSource(s.Asset, source, visit); err != nil {
					return err
				}
			}
		}
		for _, list := range t.Children() {
			if err := fileAssets(list.NodeTasks, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// visitSource visits one file asset, creating its reference; a pinned
// download, checked at load, must match its declared digest, unless it is
// saved decompressed.
func visitSource(ref **assets.Ref, source assetSource, visit func(*assets.Ref, assetSource) error) error {
	if *ref == nil {
		*ref = &assets.Ref{}
	}
	source.sha256 = strings.ToLower(source.sha256)
	if err := visit(*ref, source); err != nil {
		return err
	}
	if source.url != "" && source.sha256 != "" && source.decompress == "" && (*ref).SHA256 != source.sha256 {
		return fmt.Errorf("download asset does not match its declared SHA-256")
	}
	return nil
}
func projectAssets(p *Project, visit func(*assets.Ref, assetSource) error) error {
	if err := WalkFleetStages(p.FleetStages, func(_ string, s FleetStage) error { return fileAssets(s.NodeTasks, visit) }); err != nil {
		return err
	}
	return moduleAssets(p.Modules, visit)
}

func moduleAssets(modules map[string]modules.Module, visit func(*assets.Ref, assetSource) error) error {
	for _, module := range modules {
		for i := range module.Files {
			file := &module.Files[i]
			if err := visit(&file.Asset, assetSource{path: file.LocalPath}); err != nil {
				return err
			}
		}
	}
	return nil
}

// KeptFileAssets lists the files of a job that a node keeps between runs, so
// that a later run sends it only the ones it lacks: the files of its modules
// that are not core, and the files from a web address that its node tasks
// install or unpack. A file from the project is sent each time: it may hold a
// secret, which the node keeps no longer than the job.
func KeptFileAssets(nodeTasks []Task, catalog map[string]modules.Module) ([]assets.Ref, error) {
	var refs []assets.Ref
	seen := map[string]bool{}
	keep := func(ref *assets.Ref) {
		if !seen[ref.SHA256] {
			seen[ref.SHA256] = true
			refs = append(refs, *ref)
		}
	}
	err := fileAssets(nodeTasks, func(ref *assets.Ref, source assetSource) error {
		if source.url != "" {
			keep(ref)
		}
		return nil
	})
	// The module traversal callback always returns nil and traversal has no other error source.
	_ = moduleAssets(catalog, func(ref *assets.Ref, _ assetSource) error { keep(ref); return nil })
	return refs, err
}

// KeptFilesOfANodeJob returns the files of a node's job that the node keeps
// (KeptFileAssets), from the node tasks of its fleet stage, which carry the
// web addresses that templates give: a node's own copy of them
// (NodeTasksFor) carries none. Of those, the job names the ones it uses.
func KeptFilesOfANodeJob(fleetStageTasks []Task, job Job) ([]assets.Ref, error) {
	kept, err := KeptFileAssets(fleetStageTasks, job.Modules)
	if err != nil {
		return nil, err
	}
	required, err := RequiredAssets(job.NodeTasks, job.Modules)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, ref := range required {
		used[ref.SHA256] = true
	}
	return slices.DeleteFunc(kept, func(ref assets.Ref) bool { return !used[ref.SHA256] }), nil
}

func jobAssets(nodeTasks []Task, modules map[string]modules.Module, visit func(*assets.Ref, assetSource) error) error {
	if err := fileAssets(nodeTasks, visit); err != nil {
		return err
	}
	return moduleAssets(modules, visit)
}

// FreezeAssets saves every file the project's tasks use in dir. A pinned
// download that an earlier run saved in one of earlierAssetDirs is reused
// after its SHA-256 is checked again, so an unchanged run fetches nothing.
// An unchecked or decompressed download is always fetched.
func FreezeAssets(ctx context.Context, p *Project, dir string, earlierAssetDirs []string) error {
	type sourceKey struct{ path, url, digest, decompress string }
	bySource := map[sourceKey]assets.Ref{}
	return projectAssets(p, func(ref *assets.Ref, source assetSource) error {
		path, template := source.path, source.template
		key := sourceKey{path, source.url, source.sha256, source.decompress}
		saved, ok := bySource[key]
		if !ok {
			var err error
			switch {
			case source.url != "" && source.decompress != "":
				// The SHA-256 is the file's as published; validation accepted
				// none only with dangerously_skip_checksum: true.
				saved, err = assets.DownloadDecompressed(ctx, dir, source.url, source.sha256, source.decompress)
				if err != nil {
					return fmt.Errorf("download %s: %w", source.url, err)
				}
			case source.url != "" && source.sha256 == "":
				// Validation accepted this only with dangerously_skip_checksum: true.
				saved, err = assets.DownloadUnchecked(ctx, dir, source.url)
				if err != nil {
					return fmt.Errorf("download %s: %w", source.url, err)
				}
			case source.url != "":
				// The pinned bytes are the same whichever run saved them.
				if reused, ok := assets.Reuse(ctx, dir, earlierAssetDirs, source.sha256); ok {
					saved = reused
					break
				}
				saved, err = assets.Download(ctx, dir, source.url, source.sha256)
				if err != nil {
					return fmt.Errorf("download %s: %w", source.url, err)
				}
			default:
				saved, err = assets.Snapshot(ctx, dir, path)
			}
			if err != nil {
				return err
			}
			bySource[key] = saved
		}
		if template != nil && fmt.Sprintf("%x", sha256.Sum256([]byte(*template))) != saved.SHA256 {
			return fmt.Errorf("template changed while preparing run: %s; run again", path)
		}
		*ref = saved
		return nil
	})
}

// BindAssets restores local paths and template text from the saved assets.
// Template parsing/reference inspection and rendering use the same frozen text.
func BindAssets(nodeTasks []Task, modules map[string]modules.Module, dir string) error {
	templates := map[string]string{}
	return jobAssets(nodeTasks, modules, func(ref *assets.Ref, source assetSource) error {
		template := source.template
		if err := ref.Validate(); err != nil {
			return err
		}
		ref.Path = filepath.Join(dir, ref.SHA256)
		if template != nil {
			text, ok := templates[ref.SHA256]
			if !ok {
				if err := assets.Verify(context.Background(), *ref); err != nil {
					return err
				}
				b, err := os.ReadFile(ref.Path)
				if err != nil {
					return err
				}
				text = string(b)
				templates[ref.SHA256] = text
			}
			*template = text
		}
		return nil
	})
}
func BindProjectAssets(p *Project, dir string) error {
	if err := WalkFleetStages(p.FleetStages, func(_ string, s FleetStage) error { return BindAssets(s.NodeTasks, nil, dir) }); err != nil {
		return err
	}
	return BindAssets(nil, p.Modules, dir)
}
func RequiredAssets(nodeTasks []Task, modules map[string]modules.Module) ([]assets.Ref, error) {
	var refs []assets.Ref
	seen := map[string]int64{}
	err := jobAssets(nodeTasks, modules, func(ref *assets.Ref, _ assetSource) error {
		if err := ref.Validate(); err != nil {
			return err
		}
		if size, ok := seen[ref.SHA256]; ok {
			if size != ref.Size {
				return fmt.Errorf("conflicting asset sizes")
			}
		} else {
			refs = append(refs, *ref)
			seen[ref.SHA256] = ref.Size
		}
		return nil
	})
	return refs, err
}
