package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
)

type taskEditor struct {
	browser                                                     *Browser
	form                                                        *fyne.Container
	titleEntry, goalEntry, descriptionEntry, specificationEntry *widget.Entry
	hostnameEntry, labelsEntry, rankEntry                       *widget.Entry
	status                                                      *widget.Label
	save, cancel                                                *widget.Button
	selectedKey                                                 string
	drafts                                                      map[string]*taskDraft
	updating                                                    bool
}

// taskDraft stores both the latest observed server snapshot and the user's
// current values. Comparing against the snapshot keeps unrelated edits out of
// the PATCH even when the visible form has remained open across refreshes.
type taskDraft struct {
	base                 storage.Task
	title                string
	goal                 string
	description          string
	specification        string
	hostname             string
	labels               string
	rank                 string
	inFlight             bool
	saveGeneration       uint64
	authoritativeVersion uint64
	error                string
}

func newTaskEditor(browser *Browser) *taskEditor {
	e := &taskEditor{browser: browser, drafts: make(map[string]*taskDraft)}
	e.titleEntry = widget.NewEntry()
	e.titleEntry.SetPlaceHolder("Title")
	e.goalEntry = widget.NewEntry()
	e.goalEntry.SetPlaceHolder("Goal")
	e.descriptionEntry = widget.NewMultiLineEntry()
	e.descriptionEntry.SetPlaceHolder("Description")
	e.specificationEntry = widget.NewMultiLineEntry()
	e.specificationEntry.SetPlaceHolder("Specification / handoff")
	e.hostnameEntry = widget.NewEntry()
	e.hostnameEntry.SetPlaceHolder("Hostname")
	e.labelsEntry = widget.NewMultiLineEntry()
	e.labelsEntry.SetPlaceHolder("One label per line")
	e.rankEntry = widget.NewEntry()
	e.rankEntry.SetPlaceHolder("Rank")
	e.status = widget.NewLabel("")
	e.status.Wrapping = fyne.TextWrapWord
	e.save = widget.NewButton("Save", e.saveTaskDraft)
	e.cancel = widget.NewButton("Cancel", e.cancelDraft)
	metadataForm := widget.NewForm(
		widget.NewFormItem("Title", e.titleEntry),
		widget.NewFormItem("Goal", e.goalEntry),
		widget.NewFormItem("Description", e.descriptionEntry),
		widget.NewFormItem("Specification", e.specificationEntry),
		widget.NewFormItem("Hostname", e.hostnameEntry),
		widget.NewFormItem("Labels", e.labelsEntry),
		widget.NewFormItem("Rank", e.rankEntry),
	)
	e.form = container.NewVBox(
		widget.NewLabelWithStyle("Edit task metadata", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		metadataForm, container.NewHBox(e.save, e.cancel), e.status,
	)
	for _, entry := range e.entries() {
		entry.OnChanged = func(string) { e.capture() }
	}
	e.refreshWidgets()
	return e
}

func (e *taskEditor) entries() []*widget.Entry {
	return []*widget.Entry{e.titleEntry, e.goalEntry, e.descriptionEntry, e.specificationEntry, e.hostnameEntry, e.labelsEntry, e.rankEntry}
}

func taskEditorKey(project, task string) string { return project + "\x00" + task }

func taskEditorValue(task storage.Task) taskDraft {
	return taskDraft{
		base: task, title: task.Title, goal: nullableText(task.Goal),
		description: nullableText(task.Description), specification: nullableText(task.Details),
		hostname: nullableText(task.Hostname), labels: encodeTaskLabels(task.Labels), rank: strconv.FormatInt(task.Rank, 10),
	}
}

func nullableText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (e *taskEditor) selectTask(project, task string) {
	e.selectedKey = ""
	if project != "" && task != "" {
		e.selectedKey = taskEditorKey(project, task)
	}
	e.refreshWidgets()
}

func (e *taskEditor) setAuthoritative(project, task string, authoritative storage.Task) {
	key := taskEditorKey(project, task)
	draft := e.drafts[key]
	if draft == nil {
		value := taskEditorValue(authoritative)
		e.drafts[key] = &value
	} else {
		oldBase := taskEditorValue(draft.base)
		fresh := taskEditorValue(authoritative)
		if draft.title == oldBase.title {
			draft.title = fresh.title
		}
		if draft.goal == oldBase.goal {
			draft.goal = fresh.goal
		}
		if draft.description == oldBase.description {
			draft.description = fresh.description
		}
		if draft.specification == oldBase.specification {
			draft.specification = fresh.specification
		}
		if draft.hostname == oldBase.hostname {
			draft.hostname = fresh.hostname
		}
		if draft.labels == oldBase.labels {
			draft.labels = fresh.labels
		}
		if draft.rank == oldBase.rank {
			draft.rank = fresh.rank
		}
		draft.base = authoritative
	}
	e.drafts[key].authoritativeVersion++
	if e.selectedKey == key {
		e.refreshWidgets()
	}
}

func (e *taskEditor) capture() {
	if e.updating || e.selectedKey == "" {
		return
	}
	draft := e.drafts[e.selectedKey]
	if draft == nil || draft.inFlight {
		return
	}
	draft.title = e.titleEntry.Text
	draft.goal = e.goalEntry.Text
	draft.description = e.descriptionEntry.Text
	draft.specification = e.specificationEntry.Text
	draft.hostname = e.hostnameEntry.Text
	draft.labels = e.labelsEntry.Text
	draft.rank = e.rankEntry.Text
	draft.error = ""
	e.status.SetText("")
	e.refreshButtons()
}

func (e *taskEditor) refreshWidgets() {
	e.updating = true
	defer func() { e.updating = false }()
	draft := e.drafts[e.selectedKey]
	ready := draft != nil
	for _, entry := range e.entries() {
		entry.SetText("")
		entry.Disable()
	}
	e.status.SetText("")
	e.save.Disable()
	e.cancel.Disable()
	if !ready {
		return
	}
	values := []string{draft.title, draft.goal, draft.description, draft.specification, draft.hostname, draft.labels, draft.rank}
	for i, entry := range e.entries() {
		entry.SetText(values[i])
		if !draft.inFlight {
			entry.Enable()
		}
	}
	e.status.SetText(draft.error)
	e.refreshButtons()
}

func (e *taskEditor) refreshButtons() {
	draft := e.drafts[e.selectedKey]
	if draft == nil || draft.inFlight {
		e.save.Disable()
		e.cancel.Disable()
		return
	}
	update, err := taskMetadataDiff(draft.base, *draft)
	if err != nil {
		e.save.Disable()
		e.status.SetText(err.Error())
		return
	}
	e.status.SetText(draft.error)
	if taskMetadataUpdateEmpty(update) {
		e.save.Disable()
	} else {
		e.save.Enable()
	}
	e.cancel.Enable()
}

func (e *taskEditor) cancelDraft() {
	if e.selectedKey == "" {
		return
	}
	draft := e.drafts[e.selectedKey]
	if draft == nil || draft.inFlight {
		return
	}
	value := taskEditorValue(draft.base)
	e.drafts[e.selectedKey] = &value
	e.refreshWidgets()
}

func (e *taskEditor) saveTaskDraft() {
	if e.selectedKey == "" {
		return
	}
	key := e.selectedKey
	draft := e.drafts[key]
	if draft == nil || draft.inFlight {
		return
	}
	update, err := taskMetadataDiff(draft.base, *draft)
	if err != nil {
		draft.error = err.Error()
		e.refreshWidgets()
		return
	}
	if taskMetadataUpdateEmpty(update) {
		return
	}
	if err := validateTaskDraft(*draft); err != nil {
		draft.error = err.Error()
		e.refreshWidgets()
		return
	}
	project, task := e.browser.selectedProject, e.browser.selectedTask
	if key != taskEditorKey(project, task) {
		return
	}
	draft.inFlight = true
	draft.saveGeneration++
	generation := draft.saveGeneration
	baseVersion := draft.authoritativeVersion
	e.refreshWidgets()
	e.browser.launch(func(ctx context.Context) {
		result, saveErr := e.browser.client.UpdateTask(ctx, project, task, update)
		e.browser.dispatch(func() { e.finishSave(key, project, task, generation, baseVersion, update, result, saveErr) })
	})
}

func (e *taskEditor) finishSave(key, project, task string, generation, baseVersion uint64, update storage.TaskMetadataUpdate, result *storage.Task, err error) {
	e.browser.mu.Lock()
	closing := e.browser.closing
	e.browser.mu.Unlock()
	if closing {
		return
	}
	draft := e.drafts[key]
	if draft == nil || draft.saveGeneration != generation {
		return
	}
	draft.inFlight = false
	if err != nil {
		draft.error = fmt.Sprintf("Save failed: %v", err)
	} else if result == nil || result.ID != task || result.ProjectIdent != project {
		draft.error = "Save response did not match the task. Refresh task details before continuing."
	} else {
		fresh := taskEditorValue(*result)
		if draft.authoritativeVersion == baseVersion {
			draft.base = *result
			draft.title, draft.goal, draft.description = fresh.title, fresh.goal, fresh.description
			draft.specification, draft.hostname, draft.labels, draft.rank = fresh.specification, fresh.hostname, fresh.labels, fresh.rank
		} else {
			// A newer detail arrived while saving. Apply only fields in the PATCH
			// to that newer snapshot so unrelated refreshed fields stay current.
			draft.base = mergeTaskMetadata(draft.base, *result, update)
			merged := taskEditorValue(draft.base)
			if update.Title != nil {
				draft.title = merged.title
			}
			if update.Goal != nil {
				draft.goal = merged.goal
			}
			if update.Description != nil {
				draft.description = merged.description
			}
			if update.Specification != nil {
				draft.specification = merged.specification
			}
			if update.Hostname != nil {
				draft.hostname = merged.hostname
			}
			if update.Labels != nil {
				draft.labels = merged.labels
			}
			if update.Rank != nil {
				draft.rank = merged.rank
			}
		}
		draft.authoritativeVersion++
		draft.error = "Task saved"
	}
	if e.selectedKey == key {
		e.refreshWidgets()
		if err == nil {
			e.browser.refreshTaskDetail()
		}
	}
}

func mergeTaskMetadata(latest, response storage.Task, update storage.TaskMetadataUpdate) storage.Task {
	if update.Title != nil {
		latest.Title = response.Title
	}
	if update.Goal != nil {
		latest.Goal = response.Goal
	}
	if update.Description != nil {
		latest.Description = response.Description
	}
	if update.Specification != nil {
		latest.Details = response.Details
	}
	if update.Hostname != nil {
		latest.Hostname = response.Hostname
	}
	if update.Labels != nil {
		latest.Labels = append([]string(nil), response.Labels...)
	}
	if update.Rank != nil {
		latest.Rank = response.Rank
	}
	return latest
}

func taskMetadataUpdateEmpty(update storage.TaskMetadataUpdate) bool {
	return update.Title == nil && update.Goal == nil && update.Description == nil && update.Specification == nil && update.Hostname == nil && update.Labels == nil && update.Rank == nil
}

func taskMetadataDiff(base storage.Task, draft taskDraft) (storage.TaskMetadataUpdate, error) {
	var update storage.TaskMetadataUpdate
	original := taskEditorValue(base)
	if draft.title != original.title {
		value := draft.title
		update.Title = &value
	}
	nullable := func(value string) (json.RawMessage, error) {
		if value == "" {
			return json.RawMessage("null"), nil
		}
		encoded, err := json.Marshal(value)
		return json.RawMessage(encoded), err
	}
	if draft.goal != original.goal {
		raw, err := nullable(draft.goal)
		if err != nil {
			return update, err
		}
		update.Goal = raw
	}
	if draft.description != original.description {
		raw, err := nullable(draft.description)
		if err != nil {
			return update, err
		}
		update.Description = raw
	}
	if draft.specification != original.specification {
		raw, err := nullable(draft.specification)
		if err != nil {
			return update, err
		}
		update.Specification = raw
	}
	if draft.hostname != original.hostname {
		raw, err := nullable(draft.hostname)
		if err != nil {
			return update, err
		}
		update.Hostname = raw
	}
	if draft.labels != original.labels {
		for _, label := range base.Labels {
			if strings.ContainsAny(label, "\r\n") {
				return update, fmt.Errorf("Labels include a line break that cannot be edited in this list; leave Labels unchanged to save other fields")
			}
		}
		labels, err := decodeTaskLabels(draft.labels)
		if err != nil {
			return update, err
		}
		raw, err := json.Marshal(labels)
		if err != nil {
			return update, err
		}
		update.Labels = raw
	}
	if draft.rank != original.rank {
		rank, err := strconv.ParseInt(strings.TrimSpace(draft.rank), 10, 64)
		if err != nil {
			return update, fmt.Errorf("Rank must be a whole number")
		}
		update.Rank = &rank
	}
	return update, nil
}

func validateTaskDraft(draft taskDraft) error {
	if _, err := strconv.ParseInt(strings.TrimSpace(draft.rank), 10, 64); err != nil {
		return fmt.Errorf("Rank must be a whole number")
	}
	if _, err := decodeTaskLabels(draft.labels); err != nil {
		return err
	}
	if draft.goal != "" {
		goal := draft.goal
		if _, err := storage.NormalizeTaskGoal(&goal); err != nil {
			return err
		}
	}
	return nil
}

func encodeTaskLabels(labels []string) string {
	return strings.Join(labels, "\n")
}

func decodeTaskLabels(text string) ([]string, error) {
	if text == "" {
		return []string{}, nil
	}
	if strings.ContainsAny(text, "\r") {
		return nil, fmt.Errorf("Labels must use one label per line")
	}
	return strings.Split(text, "\n"), nil
}
