package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type specImportRequest struct {
	Title                   string            `json:"title"`
	Labels                  []string          `json:"labels"`
	Body                    *string           `json:"body"`
	Manifest                json.RawMessage   `json:"manifest"`
	FileBodies              map[string]string `json:"file_bodies"`
	SourceDoc               *string           `json:"source_doc"`
	SourceArtifactID        *string           `json:"source_artifact_id"`
	SourceArtifactVersionID *string           `json:"source_artifact_version_id"`
	ActorDisplayName        *string           `json:"actor_display_name"`
}

type specVersionImportRequest struct {
	VersionLabel     *string           `json:"version_label"`
	ParentVersionID  *string           `json:"parent_version_id"`
	Body             *string           `json:"body"`
	Manifest         json.RawMessage   `json:"manifest"`
	FileBodies       map[string]string `json:"file_bodies"`
	SourceDoc        *string           `json:"source_doc"`
	ActorDisplayName *string           `json:"actor_display_name"`
}

type specManifestWriteError struct{ token string }

func (e *specManifestWriteError) Error() string { return e.token }

type specImportResponse struct {
	Artifact storage.Artifact        `json:"artifact"`
	Version  storage.ArtifactVersion `json:"version"`
}

type generateSpecTasksRequest struct {
	Confirmed        bool     `json:"confirmed"`
	ManifestItemIDs  []string `json:"manifest_item_ids"`
	Reporter         *string  `json:"reporter"`
	Hostname         *string  `json:"hostname"`
	ActorDisplayName *string  `json:"actor_display_name"`
}

type linkExistingSpecTaskRequest struct {
	VersionID        *string `json:"version_id"`
	ManifestItemID   string  `json:"manifest_item_id"`
	TaskID           string  `json:"task_id"`
	ActorDisplayName *string `json:"actor_display_name"`
}

type generateSpecTasksResponse struct {
	ArtifactID        string           `json:"artifact_id"`
	ArtifactVersionID string           `json:"artifact_version_id"`
	WorkflowRunID     string           `json:"workflow_run_id"`
	GeneratedTaskIDs  []string         `json:"generated_task_ids"`
	GeneratedLinkIDs  []string         `json:"generated_link_ids"`
	Items             []map[string]any `json:"items"`
	Replayed          bool             `json:"replayed"`
}

func (a *api) registerSpecManifestTaskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/specs/{artifact_id}/generate-tasks", a.generateSpecTasks)
	mux.HandleFunc("POST /v1/projects/{ident}/specs/{artifact_id}/link-task", a.linkExistingSpecTask)
}

func (a *api) generateSpecTasks(w http.ResponseWriter, r *http.Request) {
	origin, err := agentOrigin(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	agentID := strings.TrimSpace(r.Header.Get("X-Agent-Id"))
	if origin == nil && agentID != "" && agentID != "_default" {
		writeError(w, http.StatusBadRequest, "agent task creation requires complete X-Agent session/instance/provider/OS headers")
		return
	}
	mutation, authorization, ok := a.beginArtifactMutation(w, r,
		"task.generate_from_spec", "workflow_run.start", "link.write")
	if !ok {
		return
	}
	var body generateSpecTasksRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	if !body.Confirmed {
		writeError(w, http.StatusBadRequest, "task_generation_confirmation_required")
		return
	}
	project, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	detail, err := a.store.ArtifactDetail(r.Context(), project, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if detail.Artifact.Kind != "spec" {
		writeError(w, http.StatusBadRequest, "artifact is not a spec")
		return
	}
	if detail.AcceptedVersion == nil {
		writeError(w, http.StatusBadRequest, "accepted spec version required")
		return
	}
	accepted := *detail.AcceptedVersion
	manifest, err := decodeSpecManifest(artifactID, accepted)
	if err != nil {
		writeSpecManifestReadError(w, err)
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	readSet, _ := json.Marshal(map[string][]string{"artifact_version_ids": {accepted.ArtifactVersionID}})
	phase := "generate_tasks"
	workflow, err := a.store.StartWorkflowRun(r.Context(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_task_generation", Phase: &phase,
		CoordinatorActorID: actor.ActorID, ParticipantActorIDs: []string{},
		SourceArtifactVersionID: &accepted.ArtifactVersionID, ReadSet: readSet,
		IdempotencyKey: &mutation.idempotencyKey, IsResumable: true,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if workflow == nil || workflow.Record == nil {
		writeError(w, http.StatusInternalServerError, "workflow run creation was not confirmed")
		return
	}
	run := workflow.Record
	if run.State == "cancelled" || (run.State == "failed" && !run.IsResumable) {
		writeArtifactWriteError(w, errWorkflowRunNotRetryable(run))
		return
	}
	links, err := a.store.ListArtifactLinks(r.Context(), project, storage.ArtifactLinkFilter{
		LinkType: "task_generated_from_spec", SourceKind: "artifact_version",
		SourceID: accepted.ArtifactVersionID, TargetKind: "task",
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	existingLinks := make(map[string]storage.ArtifactLink, len(links))
	for _, link := range links {
		if link.SourceChildAddress != nil {
			existingLinks[*link.SourceChildAddress] = link
		}
	}
	selected := make(map[string]struct{}, len(body.ManifestItemIDs))
	for _, id := range body.ManifestItemIDs {
		selected[id] = struct{}{}
	}
	taskIDs := nonNilStringSlice(run.GeneratedTaskIDs)
	linkIDs := nonNilStringSlice(run.GeneratedLinkIDs)
	items := make([]map[string]any, 0)
	reporter := agentID
	if body.Reporter != nil {
		reporter = *body.Reporter
	}
	for _, item := range manifest.Items {
		if len(selected) != 0 {
			if _, ok := selected[item.ManifestItemID]; !ok {
				continue
			}
		}
		childAddress := "manifest.items[" + item.ManifestItemID + "]"
		if link, ok := existingLinks[childAddress]; ok {
			taskIDs = appendSpecTaskID(taskIDs, link.TargetID)
			linkIDs = appendSpecTaskID(linkIDs, link.LinkID)
			items = append(items, map[string]any{"manifest_item_id": item.ManifestItemID, "task_id": link.TargetID, "link_id": link.LinkID, "reused": true})
			continue
		}
		var task *storage.Task
		if item.GatewayTaskID != nil {
			existing, err := a.store.Task(r.Context(), project, *item.GatewayTaskID)
			if err != nil {
				writeArtifactWriteError(w, err)
				return
			}
			if existing != nil {
				task = &existing.Task
			}
		}
		if task == nil {
			labels := append([]string{}, item.Labels...)
			if !containsString(labels, "generated-from-spec") {
				labels = append(labels, "generated-from-spec")
			}
			if item.Team != nil {
				labels = append(labels, *item.Team)
			}
			title := item.Title
			if item.TaskCode != nil {
				title = *item.TaskCode + " " + item.Title
			}
			description := "Generated from spec artifact " + artifactID + " version " + accepted.ArtifactVersionID + " manifest item " + item.ManifestItemID + "."
			details := generatedSpecTaskDetails(artifactID, accepted.ArtifactVersionID, item)
			taskInput := storage.TaskInsert{
				Project: project, Title: title, Description: &description, Details: &details,
				Labels: labels, Hostname: body.Hostname, Reporter: reporter,
			}
			task, err = a.store.FindOrCreateSpecSourceTask(r.Context(), project, artifactID, accepted.ArtifactVersionID, item.ManifestItemID, taskInput, origin)
			if err != nil {
				writeArtifactWriteError(w, err)
				return
			}
		}
		itemKey := specItemGenerationKey(accepted.ArtifactVersionID, item.ManifestItemID)
		link, err := a.store.CreateSpecTaskArtifactLink(r.Context(), project, storage.ArtifactLinkInsert{
			LinkType: "task_generated_from_spec", SourceKind: "artifact_version", SourceID: accepted.ArtifactVersionID,
			SourceVersionID: &accepted.ArtifactVersionID, SourceChildAddress: &childAddress,
			TargetKind: "task", TargetID: task.ID, TargetVersionID: &accepted.ArtifactVersionID,
			CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &run.WorkflowRunID, IdempotencyKey: &itemKey,
		})
		if err != nil {
			writeArtifactWriteError(w, err)
			return
		}
		if link == nil || link.Record == nil {
			writeError(w, http.StatusInternalServerError, "artifact link was not confirmed")
			return
		}
		if !link.Replayed {
			comment := "Generated from spec artifact `" + artifactID + "` accepted version `" + accepted.ArtifactVersionID + "` manifest item `" + item.ManifestItemID + "`. Compatibility fallback: the current task schema has no dedicated source fields, so the source tuple is embedded in this task's specification/details and mirrored by artifact link `" + link.Record.LinkID + "`."
			if _, err := a.store.AddTaskComment(r.Context(), project, task.ID, "system", "system", comment, nil, origin); err != nil {
				writeArtifactWriteError(w, err)
				return
			}
		}
		taskIDs = appendSpecTaskID(taskIDs, task.ID)
		linkIDs = appendSpecTaskID(linkIDs, link.Record.LinkID)
		items = append(items, map[string]any{"manifest_item_id": item.ManifestItemID, "task_id": task.ID, "link_id": link.Record.LinkID, "reused": link.Replayed})
	}
	succeeded := "succeeded"
	noFailure := (*string)(nil)
	clearFailure := &noFailure
	updated, err := a.store.UpdateWorkflowRun(r.Context(), run.WorkflowRunID, storage.WorkflowRunUpdate{
		State: &succeeded, FailureReason: clearFailure,
		GeneratedContributionIDs: stringSlicePointer(run.GeneratedContributionIDs),
		GeneratedVersionIDs:      stringSlicePointer(run.GeneratedVersionIDs),
		GeneratedTaskIDs:         &taskIDs, GeneratedLinkIDs: &linkIDs,
		GeneratedChunkIDs: stringSlicePointer(run.GeneratedChunkIDs),
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "workflow_run not found")
		return
	}
	output := generateSpecTasksResponse{
		ArtifactID: artifactID, ArtifactVersionID: accepted.ArtifactVersionID, WorkflowRunID: updated.WorkflowRunID,
		GeneratedTaskIDs: taskIDs, GeneratedLinkIDs: linkIDs, Items: items, Replayed: workflow.Replayed,
	}
	writeArtifactMutation(w, artifactMutationStatus(workflow.Replayed), output, actor, mutation, authorization,
		map[string]any{"workflow_run_id": output.WorkflowRunID, "generated_task_ids": taskIDs, "generated_link_ids": linkIDs},
		workflow.Replayed, nil)
}

func (a *api) linkExistingSpecTask(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r,
		"task.generate_from_spec", "workflow_run.start", "link.write")
	if !ok {
		return
	}
	var body linkExistingSpecTaskRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	itemID, taskID := strings.TrimSpace(body.ManifestItemID), strings.TrimSpace(body.TaskID)
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "manifest_item_id_required")
		return
	}
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id_required")
		return
	}
	project, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	detail, err := a.store.ArtifactDetail(r.Context(), project, artifactID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if detail.Artifact.Kind != "spec" {
		writeError(w, http.StatusBadRequest, "artifact is not a spec")
		return
	}
	taskDetail, err := a.store.Task(r.Context(), project, taskID)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if taskDetail == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	version := detail.AcceptedVersion
	if body.VersionID != nil {
		version, err = a.store.ArtifactVersion(r.Context(), project, artifactID, strings.TrimSpace(*body.VersionID))
		if err != nil {
			writeArtifactWriteError(w, err)
			return
		}
		if version == nil {
			writeError(w, http.StatusNotFound, "version not found")
			return
		}
	}
	if version == nil {
		writeError(w, http.StatusBadRequest, "accepted spec version required")
		return
	}
	manifest, err := decodeSpecManifest(artifactID, *version)
	if err != nil {
		writeSpecManifestReadError(w, err)
		return
	}
	found := false
	for _, item := range manifest.Items {
		if item.ManifestItemID == itemID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "manifest_item not found")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	readSet, _ := json.Marshal(map[string][]string{"artifact_version_ids": {version.ArtifactVersionID}})
	phase := "link_existing_task"
	workflow, err := a.store.StartWorkflowRun(r.Context(), storage.WorkflowRunInsert{
		ArtifactID: artifactID, WorkflowKind: "spec_task_generation", Phase: &phase,
		CoordinatorActorID: actor.ActorID, ParticipantActorIDs: []string{},
		SourceArtifactVersionID: &version.ArtifactVersionID, ReadSet: readSet,
		IdempotencyKey: &mutation.idempotencyKey, IsResumable: true,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if workflow == nil || workflow.Record == nil {
		writeError(w, http.StatusInternalServerError, "workflow run creation was not confirmed")
		return
	}
	if workflow.Record.State == "cancelled" || (workflow.Record.State == "failed" && !workflow.Record.IsResumable) {
		writeArtifactWriteError(w, errWorkflowRunNotRetryable(workflow.Record))
		return
	}
	childAddress := "manifest.items[" + itemID + "]"
	itemKey := specItemGenerationKey(version.ArtifactVersionID, itemID)
	link, err := a.store.CreateArtifactLink(r.Context(), storage.ArtifactLinkInsert{
		LinkType: "task_generated_from_spec", SourceKind: "artifact_version", SourceID: version.ArtifactVersionID,
		SourceVersionID: &version.ArtifactVersionID, SourceChildAddress: &childAddress,
		TargetKind: "task", TargetID: taskID, TargetVersionID: &version.ArtifactVersionID,
		CreatedByActorID: actor.ActorID, CreatedViaWorkflowRunID: &workflow.Record.WorkflowRunID, IdempotencyKey: &itemKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if link == nil || link.Record == nil {
		writeError(w, http.StatusInternalServerError, "artifact link was not confirmed")
		return
	}
	taskIDs := appendSpecTaskID(nonNilStringSlice(workflow.Record.GeneratedTaskIDs), taskID)
	linkIDs := appendSpecTaskID(nonNilStringSlice(workflow.Record.GeneratedLinkIDs), link.Record.LinkID)
	succeeded := "succeeded"
	noFailure := (*string)(nil)
	updated, err := a.store.UpdateWorkflowRun(r.Context(), workflow.Record.WorkflowRunID, storage.WorkflowRunUpdate{
		State: &succeeded, FailureReason: &noFailure,
		GeneratedContributionIDs: stringSlicePointer(workflow.Record.GeneratedContributionIDs),
		GeneratedVersionIDs:      stringSlicePointer(workflow.Record.GeneratedVersionIDs),
		GeneratedTaskIDs:         &taskIDs, GeneratedLinkIDs: &linkIDs,
		GeneratedChunkIDs: stringSlicePointer(workflow.Record.GeneratedChunkIDs),
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "workflow_run not found")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(link.Replayed), link.Record, actor, mutation, authorization,
		map[string]any{"link_id": link.Record.LinkID}, link.Replayed, link.Warnings)
}

func writeSpecManifestReadError(w http.ResponseWriter, err error) {
	var readErr *specManifestReadError
	if errors.As(err, &readErr) {
		writeError(w, readErr.status, readErr.token)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func generatedSpecTaskDetails(artifactID, versionID string, item specManifestItem) string {
	var out strings.Builder
	out.WriteString("Source:\nsource_spec_artifact_id: ")
	out.WriteString(artifactID)
	out.WriteString("\nsource_spec_version_id: ")
	out.WriteString(versionID)
	out.WriteString("\nmanifest_item_id: ")
	out.WriteString(item.ManifestItemID)
	out.WriteString("\nspec_file: ")
	if item.SpecFile != nil {
		out.WriteString(*item.SpecFile)
	}
	out.WriteString("\n\n")
	if item.Team != nil {
		out.WriteString("Team: ")
		out.WriteString(*item.Team)
		out.WriteByte('\n')
	}
	if len(item.Dependencies) > 0 {
		out.WriteString("Depends on: ")
		out.WriteString(strings.Join(item.Dependencies, ", "))
		out.WriteByte('\n')
	}
	writeSpecTaskList(&out, "\nTouch surface:\n", item.TouchSurface)
	writeSpecTaskList(&out, "\nAcceptance criteria:\n", item.AcceptanceCriteria)
	writeSpecTaskList(&out, "\nValidation plan:\n", item.ValidationPlan)
	if item.SpecBody != nil {
		out.WriteString("\nFocused spec:\n")
		out.WriteString(*item.SpecBody)
		if !strings.HasSuffix(*item.SpecBody, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func writeSpecTaskList(out *strings.Builder, heading string, values []string) {
	if len(values) == 0 {
		return
	}
	out.WriteString(heading)
	for _, value := range values {
		out.WriteString("- ")
		out.WriteString(value)
		out.WriteByte('\n')
	}
}

func specItemGenerationKey(versionID, itemID string) string {
	hash := sha256.Sum256([]byte(versionID + ":" + itemID))
	return "spec-task-" + hex.EncodeToString(hash[:])
}

func nonNilStringSlice(values []string) []string { return append([]string{}, values...) }

func stringSlicePointer(values []string) *[]string {
	copy := nonNilStringSlice(values)
	return &copy
}

func appendSpecTaskID(values []string, id string) []string {
	for _, value := range values {
		if value == id {
			return values
		}
	}
	return append(values, id)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func errWorkflowRunNotRetryable(run *storage.WorkflowRun) error {
	if run.State == "cancelled" {
		return errors.New("cancelled workflow_run is not retryable")
	}
	return errors.New("non-resumable failed workflow_run is not retryable")
}

const specManifestStabilityPolicyJSON = `{"stable_id_field":"manifest_item_id","fallback_heuristic":"phase_id plus task id/code from source-adjacent manifests","unchanged":"preserve manifest_item_id","renamed":"preserve manifest_item_id when the task id/code is unchanged","split":"keep the original manifest_item_id for the continuing item and issue new ids for split-out work","merged":"issue a new manifest_item_id unless the manifest explicitly declares the surviving id","deleted":"do not delete existing gateway task or artifact links; future generations simply omit the item","collision":"explicit duplicate manifest_item_id values are rejected"}`

func specManifestStabilityPolicy() json.RawMessage {
	return json.RawMessage(specManifestStabilityPolicyJSON)
}

func (a *api) createSpec(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact.write", "artifact_version.create")
	if !ok {
		return
	}
	var body specImportRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title_required")
		return
	}
	payload, err := buildSpecManifestPayload(body.Manifest, body.FileBodies, body.SourceDoc, body.SourceArtifactID, body.SourceArtifactVersionID)
	if err != nil {
		writeSpecManifestError(w, err)
		return
	}
	if a.disableArtifactBodySchema && len(payload) != 0 {
		writeError(w, http.StatusServiceUnavailable, "artifact_body_schema_disabled")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	projectIdent := r.PathValue("ident")
	subkind := "implementation"
	initialVersionKey := mutation.idempotencyKey + ":initial-version"
	initialVersionLabel := "imported"
	sourceFormat := "gateway-spec-directory"
	artifactWrite, versionWrite, err := a.store.CreateSpecArtifactWithInitialVersion(r.Context(), storage.ArtifactInsert{
		ProjectIdent: projectIdent, Kind: "spec", Subkind: &subkind, Title: title,
		Labels: body.Labels, CreatedByActorID: actor.ActorID,
	}, storage.ArtifactVersionInsert{
		VersionLabel: &initialVersionLabel, BodyFormat: "markdown", Body: body.Body,
		StructuredPayload: payload, SourceFormat: &sourceFormat, CreatedByActorID: actor.ActorID,
		VersionState: "draft", IdempotencyKey: &initialVersionKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	warnings := append(append([]storage.ArtifactQuotaWarning(nil), artifactWrite.Warnings...), versionWrite.Warnings...)
	data := specImportResponse{Artifact: *artifactWrite.Record, Version: *versionWrite.Record}
	writeArtifactMutation(w, http.StatusCreated, data, actor, mutation, authorization,
		map[string]any{"artifact_id": data.Artifact.ArtifactID, "artifact_version_id": data.Version.ArtifactVersionID},
		false, warnings)
}

func (a *api) createSpecVersion(w http.ResponseWriter, r *http.Request) {
	mutation, authorization, ok := a.beginArtifactMutation(w, r, "artifact_version.create")
	if !ok {
		return
	}
	var body specVersionImportRequest
	if !readJSONRequest(w, r, &body, artifactMutationBodyLimit) {
		return
	}
	payload, err := buildSpecManifestPayload(body.Manifest, body.FileBodies, body.SourceDoc, nil, nil)
	if err != nil {
		writeSpecManifestError(w, err)
		return
	}
	if a.disableArtifactBodySchema && len(payload) != 0 {
		writeError(w, http.StatusServiceUnavailable, "artifact_body_schema_disabled")
		return
	}
	projectIdent, artifactID := r.PathValue("ident"), r.PathValue("artifact_id")
	artifact, ok := a.loadProjectArtifactForMutation(w, r, projectIdent, artifactID)
	if !ok {
		return
	}
	if artifact.Kind != "spec" {
		writeError(w, http.StatusBadRequest, "artifact is not a spec")
		return
	}
	actor, err := a.resolveArtifactMutationActor(r.Context(), mutation, body.ActorDisplayName)
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	sourceFormat := "gateway-spec-directory"
	write, err := a.store.CreateArtifactVersionWithWorkflow(r.Context(), projectIdent, artifactID, storage.ArtifactVersionInsert{
		VersionLabel: body.VersionLabel, ParentVersionID: body.ParentVersionID, BodyFormat: "markdown", Body: body.Body,
		StructuredPayload: payload, SourceFormat: &sourceFormat, CreatedByActorID: actor.ActorID,
		CreatedViaWorkflowRunID: mutation.workflowRunID, VersionState: "draft", IdempotencyKey: &mutation.idempotencyKey,
	})
	if err != nil {
		writeArtifactWriteError(w, err)
		return
	}
	if write == nil || write.Record == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeArtifactMutation(w, artifactMutationStatus(write.Replayed), write.Record, actor, mutation, authorization,
		map[string]any{"artifact_version_id": write.Record.ArtifactVersionID, "workflow_run_id": mutation.workflowRunID}, write.Replayed, write.Warnings)
}

func buildSpecManifestPayload(raw json.RawMessage, fileBodies map[string]string, sourceDoc, sourceArtifactID, sourceArtifactVersionID *string) (json.RawMessage, error) {
	items, err := normalizeSpecManifest(raw, fileBodies)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Schema                  string             `json:"schema"`
		SourceDoc               *string            `json:"source_doc"`
		SourceArtifactID        *string            `json:"source_artifact_id"`
		SourceArtifactVersionID *string            `json:"source_artifact_version_id"`
		Manifest                json.RawMessage    `json:"manifest"`
		Items                   []specManifestItem `json:"items"`
		StabilityPolicy         json.RawMessage    `json:"stability_policy"`
	}{"gateway.spec_manifest.v1", sourceDoc, sourceArtifactID, sourceArtifactVersionID, raw, items, specManifestStabilityPolicy()})
}

func normalizeSpecManifest(raw json.RawMessage, fileBodies map[string]string) ([]specManifestItem, error) {
	var manifest map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &manifest) != nil || manifest == nil {
		return nil, &specManifestWriteError{token: "manifest_items_required"}
	}
	itemsRaw, hasItems := rawArray(manifest["items"])
	items := make([]specManifestItem, 0)
	seen := make(map[string]struct{})
	if hasItems {
		for _, rawItem := range itemsRaw {
			item, err := normalizeSpecManifestItem(rawItem, nil, nil, fileBodies)
			if err != nil {
				return nil, err
			}
			if err := appendSpecManifestItem(&items, seen, item); err != nil {
				return nil, err
			}
		}
	} else if phases, ok := rawArray(manifest["phases"]); ok {
		for _, rawPhase := range phases {
			var phase map[string]json.RawMessage
			if json.Unmarshal(rawPhase, &phase) != nil {
				continue
			}
			phaseID := optionalSpecString(phase, "id")
			phaseName := optionalSpecString(phase, "name")
			if phaseName == nil {
				phaseName = phaseID
			}
			tasks, ok := rawArray(phase["tasks"])
			if !ok {
				continue
			}
			for _, rawTask := range tasks {
				item, err := normalizeSpecManifestItem(rawTask, phaseID, phaseName, fileBodies)
				if err != nil {
					return nil, err
				}
				if err := appendSpecManifestItem(&items, seen, item); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(items) == 0 {
		return nil, &specManifestWriteError{token: "manifest_items_required"}
	}
	return items, nil
}

func normalizeSpecManifestItem(raw json.RawMessage, phaseID, phaseName *string, fileBodies map[string]string) (specManifestItem, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return specManifestItem{}, &specManifestWriteError{token: "manifest_item_id_required"}
	}
	itemID := optionalSpecString(fields, "manifest_item_id", "stable_id")
	if itemID == nil {
		fallback := optionalSpecString(fields, "id", "task_code", "code")
		if fallback != nil {
			if phaseID != nil {
				id := *phaseID + ":" + *fallback
				itemID = &id
			} else {
				itemID = fallback
			}
		}
	}
	if itemID == nil {
		return specManifestItem{}, &specManifestWriteError{token: "manifest_item_id_required"}
	}
	title := optionalSpecString(fields, "title")
	if title == nil {
		return specManifestItem{}, &specManifestWriteError{token: "task_title_required"}
	}
	phase := phaseName
	if phase == nil {
		phase = optionalSpecString(fields, "phase", "phase_id")
	}
	specFile := optionalSpecString(fields, "spec_file", "file_path")
	specBody := optionalSpecString(fields, "spec_body", "body")
	if specBody == nil && specFile != nil {
		if content, ok := fileBodies[*specFile]; ok {
			specBody = &content
		}
	}
	arrays := func(keys ...string) []string {
		value := specStringArray(fields, keys...)
		if value == nil {
			return []string{}
		}
		return value
	}
	return specManifestItem{
		ManifestItemID: *itemID, Phase: phase, TaskCode: optionalSpecString(fields, "task_code", "id", "code"),
		Team: optionalSpecString(fields, "team"), Title: *title, Status: optionalSpecString(fields, "status"),
		Dependencies: arrays("dependencies", "depends_on"), Labels: arrays("labels"),
		TouchSurface: arrays("touch_surface"), AcceptanceCriteria: arrays("acceptance_criteria"),
		ValidationPlan: arrays("validation_plan"), GatewayTaskID: optionalSpecString(fields, "gateway_task_id"),
		SpecFile: specFile, SpecBody: specBody, Metadata: append(json.RawMessage(nil), raw...),
	}, nil
}

func appendSpecManifestItem(items *[]specManifestItem, seen map[string]struct{}, item specManifestItem) error {
	if _, exists := seen[item.ManifestItemID]; exists {
		return &specManifestWriteError{token: "duplicate_manifest_item_id"}
	}
	seen[item.ManifestItemID] = struct{}{}
	*items = append(*items, item)
	return nil
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	var result []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, false
	}
	return result, true
}

func optionalSpecString(fields map[string]json.RawMessage, keys ...string) *string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(fields[key], &value) == nil {
			value = strings.TrimSpace(value)
			if value != "" {
				return &value
			}
		}
	}
	return nil
}

func specStringArray(fields map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		raw := fields[key]
		if values, ok := rawArray(raw); ok {
			result := make([]string, 0, len(values))
			for _, item := range values {
				var value string
				if json.Unmarshal(item, &value) == nil {
					if value = strings.TrimSpace(value); value != "" {
						result = append(result, value)
					}
				}
			}
			return result
		}
		if value := optionalSpecString(map[string]json.RawMessage{key: raw}, key); value != nil {
			return []string{*value}
		}
	}
	return nil
}

func writeSpecManifestError(w http.ResponseWriter, err error) {
	var manifestErr *specManifestWriteError
	if errors.As(err, &manifestErr) {
		writeError(w, http.StatusBadRequest, manifestErr.token)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_spec_manifest")
}
