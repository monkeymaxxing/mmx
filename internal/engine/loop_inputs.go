package engine

import (
	"mmx/internal/config"
	"mmx/internal/value"
)

func boundScope(parent value.Scope, task config.Task) value.Scope {
	if len(task.Bindings) == 0 {
		return parent
	}
	local := parent.Local()
	for key, v := range task.Bindings {
		local[key] = v
	}
	return local
}

// fixTracked resolves, in the block's entry scope, exactly the node tasks config.EntryNodeTasks
// says a service task block fixes as it starts, in place. A tracked loop keeps
// its entry items as Prepared node tasks, still grouped under the loop so its named
// result contains every item. It returns the resolved node tasks by phase, in
// written order. Every other task, loops included, evaluates when it runs.
// whenIsFalse decides a tracked task's when as the block starts. A task whose
// when is false then writes nothing, so a value that only its arguments
// read need not exist, such as an input that another kind of server gets:
// its arguments that do not resolve leave it out of what the block fixes.
func fixTracked(op *config.Task, s value.Scope, whenIsFalse func(config.Task, value.Scope) bool) (map[string][]*config.Task, error) {
	entries, err := config.EntryNodeTasks(*op, s, false)
	if err != nil {
		return nil, err
	}
	block := *op.TaskBlock
	op.TaskBlock = &block
	op.TaskBlock.Changes = append([]config.Task(nil), op.TaskBlock.Changes...)
	op.TaskBlock.Activate = append([]config.Task(nil), op.TaskBlock.Activate...)
	phases := map[string][]config.Task{"changes": op.TaskBlock.Changes, "activate": op.TaskBlock.Activate}
	fixed := map[string][]*config.Task{}
	for _, e := range entries {
		task := &phases[e.Phase][e.Index] //nolint:nilaway // EntryNodeTasks builds phase indices from the same slices copied into phases.
		if task.Loop == nil {
			a, err := config.ResolveArguments(*task, s)
			if err != nil && whenIsFalse(*task, s) {
				task.WhenWasFalseAtBlockStart = true
				continue
			}
			if err != nil {
				return nil, err
			}
			*task = a
			fixed[e.Phase] = append(fixed[e.Phase], task)
			continue
		}
		task.Prepared = make([]config.Task, len(e.Items))
		for i := range e.Items {
			child := e.Item(i)
			a, err := config.ResolveArguments(child, boundScope(s, child))
			if err != nil && whenIsFalse(child, boundScope(s, child)) {
				child.WhenWasFalseAtBlockStart = true
				task.Prepared[i] = child
				continue
			}
			if err != nil {
				return nil, err
			}
			task.Prepared[i] = a
			fixed[e.Phase] = append(fixed[e.Phase], &task.Prepared[i])
		}
	}
	return fixed, nil
}
