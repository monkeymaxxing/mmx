package config

// checkWithinFleetStages lets each module check a task against the earlier
// tasks of its fleet stage, such as two package tasks that would undo each
// other on every run.
func checkWithinFleetStages(stages []FleetStage) error {
	for _, s := range stages {
		record := map[string]any{}
		// A task under a condition, or inside a role call or task block under
		// one, is conditional.
		var walk func(nodeTasks []Task, conditional bool) error
		walk = func(nodeTasks []Task, conditional bool) error {
			for _, t := range nodeTasks {
				conditional := conditional || t.When != nil
				name, inputs := t.ModuleCall()
				if m, ok := inputs.(interface {
					CheckWithinFleetStage(map[string]any, bool) error
				}); ok {
					if err := m.CheckWithinFleetStage(record, conditional); err != nil {
						return t.Sources.Error(name, err)
					}
				}
				for _, list := range t.Children() {
					if err := walk(list.NodeTasks, conditional); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(s.NodeTasks, false); err != nil {
			return err
		}
		if err := checkWithinFleetStages(s.FleetStages); err != nil {
			return err
		}
	}
	return nil
}
