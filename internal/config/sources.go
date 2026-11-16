package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mmx/internal/modules"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Location is a YAML position. File is relative to the project directory; a
// default setting has only a Field naming where it comes from.
type Location struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Field  string `json:"field"`
}

func (s Location) String() string {
	if s.File == "" {
		return s.Field
	}
	return fmt.Sprintf("%s:%d:%d (%s)", s.File, s.Line, s.Column, s.Field)
}

type Sources map[string]Location

func (s Sources) At(field string) Location {
	for {
		if at, ok := s[field]; ok {
			return at
		}
		i := strings.LastIndexAny(field, ".[")
		if i < 0 {
			break
		}
		field = field[:i]
	}
	return s[""]
}

func (s Sources) Error(field string, err error) error {
	if err == nil {
		return nil
	}
	var specific *modules.FieldError
	if errors.As(err, &specific) {
		field = fieldPath(field, specific.Field)
		err = specific.Err
	}
	if at := s.At(field); at.File != "" {
		return fmt.Errorf("%s: %w", at, err)
	}
	return fmt.Errorf("%s: %w", field, err)
}

// Capture locations beside the owning definitions, never inside user values.
// A nested definition owns its own fields, avoiding duplicate metadata in jobs.
func captureSources(v reflect.Value, n *yaml.Node, file, field string) Sources {
	if n.Kind == yaml.DocumentNode {
		return captureSources(v, n.Content[0], file, field)
	}
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			v = reflect.Value{}
			break
		}
		v = v.Elem()
	}
	locations := Sources{"": {File: file, Line: n.Line, Column: n.Column, Field: field}}
	add := func(key string, child reflect.Value, node *yaml.Node) {
		for suffix, at := range captureSources(child, node, file, fieldPath(field, key)) {
			locations[fieldPath(key, suffix)] = at
		}
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key, node := n.Content[i].Value, n.Content[i+1]
			var child reflect.Value
			if v.IsValid() {
				switch v.Kind() {
				case reflect.Struct:
					for j := 0; j < v.NumField(); j++ {
						if strings.Split(v.Type().Field(j).Tag.Get("yaml"), ",")[0] == key {
							child = v.Field(j)
							break
						}
					}
				case reflect.Map:
					if item := v.MapIndex(reflect.ValueOf(key)); item.IsValid() {
						child = reflect.New(item.Type()).Elem()
						child.Set(item)
					}
				default:
					// Non-struct, non-map kinds have no mapping fields; YAML child locations are still captured.
				}
			}
			add(key, child, node)
			if v.IsValid() && v.Kind() == reflect.Map && child.IsValid() {
				v.SetMapIndex(reflect.ValueOf(key), child)
			}
		}
	case yaml.SequenceNode:
		for i, node := range n.Content {
			var child reflect.Value
			if v.IsValid() && v.Kind() == reflect.Slice && i < v.Len() {
				child = v.Index(i)
			}
			add(fmt.Sprintf("[%d]", i), child, node)
		}
	default:
		// Scalar and alias nodes have no child fields; document nodes were unwrapped above.
	}
	if v.IsValid() && v.Kind() == reflect.Struct {
		if owned := v.FieldByName("Sources"); owned.IsValid() && owned.CanSet() && owned.Type() == reflect.TypeOf(Sources{}) {
			owned.Set(reflect.ValueOf(locations))
			return Sources{"": locations[""]}
		}
	}
	return locations
}

func fieldPath(parent, child string) string {
	switch {
	case parent == "":
		return child
	case child == "":
		return parent
	case strings.HasPrefix(child, "["):
		return parent + child
	}
	return parent + "." + child
}

// Saved definitions and node jobs carry every field's location. A definition's
// locations share one file, and each field path extends the definition's own,
// so JSON stores the file and base path once and each field's line and column.
// Other locations, such as inherited timing defaults, are stored whole.
type savedSources struct {
	File  string              `json:"file,omitempty"`
	Field string              `json:"field,omitempty"`
	At    map[string][2]int   `json:"at,omitempty"`
	Other map[string]Location `json:"other,omitempty"`
}

func (s Sources) MarshalJSON() ([]byte, error) {
	root := s[""]
	saved := savedSources{File: root.File, Field: root.Field, At: map[string][2]int{}, Other: map[string]Location{}}
	for key, at := range s {
		if at.File == root.File && at.Field == fieldPath(root.Field, key) {
			saved.At[key] = [2]int{at.Line, at.Column}
		} else {
			saved.Other[key] = at
		}
	}
	return json.Marshal(saved)
}

// Saved JSON is strict, like protocol.Decode: unknown fields are rejected.
func (s *Sources) UnmarshalJSON(b []byte) error {
	var saved savedSources
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return err
	}
	*s = Sources{}
	for key, at := range saved.At {
		(*s)[key] = Location{File: saved.File, Line: at[0], Column: at[1], Field: fieldPath(saved.Field, key)}
	}
	for key, at := range saved.Other {
		(*s)[key] = at
	}
	return nil
}

func (s Sources) Sub(prefix string) Sources {
	out := Sources{"": s.At(prefix)}
	for key, at := range s {
		if strings.HasPrefix(key, prefix+".") {
			out[strings.TrimPrefix(key, prefix+".")] = at
		}
	}
	return out
}
