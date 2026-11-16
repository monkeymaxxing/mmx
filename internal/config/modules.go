package config

import (
	"mmx/internal/modules"
	"strings"
)

func prepareModule(t *Task, base string) error {
	for _, s := range taskSources(t) {
		if s.Path != nil && modules.Dynamic(*s.Path) {
			if err := modules.CheckSourceReferences(s.Field[strings.LastIndex(s.Field, ".")+1:], *s.Path); err != nil {
				return err
			}
		}
	}
	if _, inputs := t.ModuleCall(); inputs != nil {
		return inputs.(modules.Inputs).Prepare(base)
	}
	return nil
}
