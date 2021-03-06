/*
Copyright 2017 The OpenEBS Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package task

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
	"github.com/openebs/maya/pkg/apis/openebs.io/v1alpha1"
	"github.com/openebs/maya/pkg/template"
	"github.com/openebs/maya/pkg/util"
)

// redactJsonResult will update the provided map by removing the original json
// result doc i.e. bytes and replace it with "--redacted--"
//
// NOTE:
//  This should be done once the task group runner has finished executing all
// its tasks or when executing a task met an error & is now exiting by logging
// the error.
func redactJsonResult(templateValues map[string]interface{}) {
	templateValues[string(v1alpha1.CurrentJSONResultTLP)] = "--redacted--"
}

// PostTaskRunFn is a closure definition that provides option
// to act on an individual task's result
type PostTaskRunFn func(taskResult map[string]interface{})

// TaskGroupRunner helps in running a set of Tasks in sequence
type TaskGroupRunner struct {
	// allTaskIDs will hold the identity of the run tasks managed by this
	// group runner
	allTaskIDs []string
	// allTasks is an array of run tasks
	allTasks []*v1alpha1.RunTask
	// outputTask holds the specs to return this group runner's
	// output in the format (i.e. specs) defined in this output run task
	outputTask *v1alpha1.RunTask
	// fallbackTemplate is the CAS Template to fallback to; is optional
	fallbackTemplate string
	// rollbacks is an array of task executor that need to be run in
	// sequence in the event of any error
	rollbacks []*taskExecutor
}

func NewTaskGroupRunner() *TaskGroupRunner {
	return &TaskGroupRunner{}
}

func (m *TaskGroupRunner) AddRunTask(runtask *v1alpha1.RunTask) (err error) {
	if runtask == nil {
		err = fmt.Errorf("nil runtask: failed to add run task")
		return
	}

	if len(runtask.Spec.Meta) == 0 {
		err = fmt.Errorf("failed to add run task: nil meta task specs found: task name '%s'", runtask.Name)
		return
	}

	m.allTasks = append(m.allTasks, runtask)
	return
}

// SetOutputTask sets this runner with a run task that will be used
// to return the output after successful execution of this runner.
//
// NOTE:
//  This output format is specified in the provided run task.
func (m *TaskGroupRunner) SetOutputTask(runtask *v1alpha1.RunTask) (err error) {
	if runtask == nil {
		err = fmt.Errorf("failed to set output task: nil run task found")
		return
	}

	if len(runtask.Spec.Meta) == 0 {
		err = fmt.Errorf("failed to set output task: nil meta task specs found: task name '%s'", runtask.Name)
		return
	}

	if len(runtask.Spec.Task) == 0 {
		err = fmt.Errorf("failed to set output task: nil task specs found: task name '%s'", runtask.Name)
		return
	}

	m.outputTask = runtask
	return
}

// SetFallback sets this runner with a fallback option in case this runner gets
// into some specific errors e.g. version mismatch error
func (m *TaskGroupRunner) SetFallback(castemplate string) {
	m.fallbackTemplate = strings.TrimSpace(castemplate)
}

// isTaskIDUnique verifies if the tasks present in this group runner
// have unique task ids.
func (m *TaskGroupRunner) isTaskIDUnique(identity string) (unique bool) {
	id := strings.ToLower(identity)

	if util.ContainsString(m.allTaskIDs, id) {
		unique = false
		return
	}

	// else add the identity for future verfications
	m.allTaskIDs = append(m.allTaskIDs, id)
	unique = true
	return
}

// planForRollback plans for rollback in case of future errors while executing
// the tasks. This will add to the list of rollback tasks
//
// NOTE:
//  This is just the planning for rollback & not actual rollback.
// In the events of issues this planning will be useful.
func (m *TaskGroupRunner) planForRollback(te *taskExecutor, objectName string) error {
	// There are cases where multiple objects may be created due to a single
	// RunTask. In such cases, object name will have comma separated list of
	// object names.
	objNames := strings.Split(objectName, ",")

	// plan the rollback for all the objects that got created
	for _, name := range objNames {
		// entire rollback plan is encapsulated in the task itself
		rte, err := te.asRollbackInstance(strings.TrimSpace(name))
		if err != nil {
			return err
		}

		if rte == nil {
			// this task does not need a rollback
			continue
		}

		m.rollbacks = append(m.rollbacks, rte)
	}

	return nil
}

// rollback will rollback the previously run operation(s)
func (m *TaskGroupRunner) rollback() {
	count := len(m.rollbacks)
	if count == 0 {
		glog.Warningf("nothing to rollback: no rollback tasks were found")
		return
	}

	glog.Warningf("will rollback previously executed runtask(s)")

	// execute the rollback tasks in **reverse order**
	for i := count - 1; i >= 0; i-- {
		err := m.rollbacks[i].ExecuteIt()
		if err != nil {
			// warn this rollback error & continue with the next rollbacks
			glog.Warningf("failed to rollback run task: '%s': error '%s'", m.rollbacks[i], err.Error())
		}
	}
}

// rollback will rollback the previously run operation(s)
func (m *TaskGroupRunner) fallback(values map[string]interface{}) (output []byte, err error) {
	glog.Warningf("task group runner will fallback to '%s'", m.fallbackTemplate)
	f, err := NewFallbackRunner(m.fallbackTemplate, values)
	if err != nil {
		return
	}

	return RunFallback(f)
}

// runATask will run a task based on the task specs & template values
func (m *TaskGroupRunner) runATask(runtask *v1alpha1.RunTask, values map[string]interface{}) (err error) {
	te, err := newTaskExecutor(runtask, values)
	if err != nil {
		// log with verbose details
		glog.Errorf("failed to initialize runtask executor: name '%s': meta yaml '%s': template values in yaml '%s': template values '%+v'", runtask.Name, runtask.Spec.Meta, template.ToYaml(values), values)
		return
	}

	// check if the task ID is unique in this group
	if !m.isTaskIDUnique(te.getTaskIdentity()) {
		return fmt.Errorf("failed to execute the run task: multiple tasks having same identity is not allowed in a group run: duplicate id '%s'", te.getTaskIdentity())
	}

	errExecute := te.Execute()

	// remove the json doc (i.e. []byte) from template values since it will not
	// be used anymore and if these template values are logged will not clutter
	// the logs
	redactJsonResult(values)

	if errExecute != nil {
		glog.Errorf("failed to execute runtask: name '%s': meta yaml '%s': task yaml '%s': template values in yaml '%s': template values '%+v'", runtask.Name, runtask.Spec.Meta, runtask.Spec.Task, template.ToYaml(values), values)
	}

	// this is planning & not the actual rollback
	errRollback := m.planForRollback(te, util.GetNestedString(values, string(v1alpha1.TaskResultTLP), te.getTaskIdentity(), string(v1alpha1.ObjectNameTRTP)))
	if errRollback != nil {
		glog.Errorf("failed to plan for rollback: '%+v'", errRollback)
	}

	// err will always contain the higher priority error
	// here errExecute > errRollback.
	if errRollback != nil {
		err = errRollback
	}
	if errExecute != nil {
		err = errExecute
	}
	return
}

// runAllTasks will run all tasks in the sequence as defined in the array
func (m *TaskGroupRunner) runAllTasks(values map[string]interface{}) (err error) {
	for _, runtask := range m.allTasks {
		err = m.runATask(runtask, values)
		if err != nil {
			return
		}
	}

	return
}

// runOutput gets the output of this runner once all the tasks were executed
// successfully
func (m *TaskGroupRunner) runOutput(values map[string]interface{}) (output []byte, err error) {

	if m.outputTask == nil || len(m.outputTask.Spec.Task) == 0 {
		// nothing needs to be done
		return
	}

	te, err := newTaskExecutor(m.outputTask, values)
	if err != nil {
		return
	}

	output, err = te.Output()
	if err != nil {
		// log with verbose details
		glog.Errorf("failed to execute output task: runtask '%+v': template values in yaml '%s': template values '%+v'", m.outputTask, template.ToYaml(values), values)
	}
	return
}

// Run will run all the defined tasks & will rollback in case of any error
//
// NOTE: values is mutated (i.e. gets modified after each task execution) to
// let the task execution result be made available to the next task before execution
// of this next task
func (m *TaskGroupRunner) Run(values map[string]interface{}) (output []byte, err error) {
	err = m.runAllTasks(values)
	if err == nil {
		return m.runOutput(values)
	}

	glog.Warningf("%+v: failed to execute runtasks", err)
	m.rollback()

	if template.IsVersionMismatch(err) && len(m.fallbackTemplate) != 0 {
		newvalues := values
		return m.fallback(newvalues)
	}

	return nil, err
}
