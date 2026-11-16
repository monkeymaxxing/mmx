package config

import (
	"mmx/internal/modules"
	"mmx/internal/value"
	"reflect"
	"sort"
	"strings"
)

// References finds runtime reads in the expanded definition. Shell source is
// literal shell code: only its explicit environment values are MMX references.
// A string that does not parse as a template is a value reference. A template
// the walker rejects, or its read of the whole scope, adds no reads here, so
// it is never delivered; validation rejects both.
func References(input any) []string {
	found := map[string]bool{}
	add := func(path string) {
		if _, err := value.Path(path); err == nil {
			found[path] = true
		}
	}
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(modules.Module{}) || v.Type() == reflect.TypeOf(modules.ModuleFile{}) || v.Type() == reflect.TypeOf(modules.DirectoryEntry{}) {
				return // Executables, file names, and support files are literal assets.
			}
			for i := 0; i < v.NumField(); i++ {
				field := v.Type().Field(i)
				if field.Name == "Sources" || field.Name == "Origins" || field.Name == "Script" || field.Name == "Dryrun" || field.Name == "Source" || field.Name == "Asset" || field.Name == "LocalPath" {
					continue
				}
				walk(v.Field(i))
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.String:
			source := v.String()
			for _, m := range value.Reference.FindAllStringSubmatch(value.WithoutLiterals(source), -1) {
				add(strings.TrimSpace(m[1]))
			}
			if strings.Contains(source, "{{") {
				reads, _ := value.TemplateReads(source)
				for _, path := range reads {
					add(path)
				}
			}
		default:
			// Boolean and numeric leaves contain no references; non-YAML kinds do not occur in definitions.
		}
	}
	walk(reflect.ValueOf(input))
	paths := make([]string, 0, len(found))
	for p := range found {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// SecretNames lists the named secrets a definition reads. Like delivery, it
// uses each reference's static prefix: .secrets.<name> loads the whole secret.
func SecretNames(input any) []string {
	found := map[string]bool{}
	for _, path := range References(input) {
		parts, _ := value.Path(path)
		if prefix := value.StaticPrefix(parts); len(prefix) > 1 && prefix[0].Name == "secrets" && prefix[1].Index == nil {
			found[prefix[1].Name] = true
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
