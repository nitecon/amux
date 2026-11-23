package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestDesignReviewCreateAndRoundRoutesMatchSourceContract(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, true)
	createPath := "/v1/projects/write-demo/design-reviews"
	createScopes := "artifact.write artifact_version.create"
	deniedCreate := artifactWriteRequest(handler, http.MethodPost, createPath,
		map[string]any{"title": "Denied"}, map[string]string{
			"Idempotency-Key": "review-create-denied", "X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.write",
		})
	if deniedCreate.Code != http.StatusForbidden || !strings.Contains(deniedCreate.Body.String(), "artifact_version.create") {
		t.Fatalf("create missing scope status=%d body=%s", deniedCreate.Code, deniedCreate.Body.String())
	}

	create := artifactWriteRequest(handler, http.MethodPost, createPath, map[string]any{
		"title": "  Gateway review  ", "labels": []string{"review"}, "body": "# Design",
		"body_format": "markdown", "source_artifact_id": "source-artifact",
		"source_artifact_version_id": "source-version", "actor_display_name": "Codex Test",
	}, map[string]string{
		"Idempotency-Key": "review-create", "X-Agent-Project": "write-demo", "X-Agent-Scopes": createScopes,
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("design-review create status=%d body=%s", create.Code, create.Body.String())
	}
	createValue := artifactWriteJSON(t, create)
	createData := createValue["data"].(map[string]any)
	artifact := createData["artifact"].(map[string]any)
	version := createData["version"].(map[string]any)
	artifactID := artifact["artifact_id"].(string)
	versionID := version["artifact_version_id"].(string)
	if artifact["kind"] != "design_review" || artifact["title"] != "Gateway review" || artifact["labels"].([]any)[0] != "review" ||
		version["version_label"] != "source" || version["version_state"] != "under_review" || version["body"] != "# Design" {
		t.Fatalf("design-review artifact/version mismatch: artifact=%#v version=%#v", artifact, version)
	}
	payload := version["structured_payload"].(map[string]any)
	source := payload["source"].(map[string]any)
	if payload["workflow"] != "design_review" || source["artifact_id"] != "source-artifact" || source["artifact_version_id"] != "source-version" {
		t.Fatalf("design-review source payload=%#v", payload)
	}
	createProvenance := createValue["provenance"].(map[string]any)
	if createProvenance["replay"] != false || createProvenance["generated_resources"].(map[string]any)["artifact_id"] != artifactID || createProvenance["generated_resources"].(map[string]any)["artifact_version_id"] != versionID {
		t.Fatalf("design-review creation provenance=%#v", createProvenance)
	}

	roundPath := "/v1/projects/write-demo/design-reviews/" + artifactID + "/rounds"
	roundBody := map[string]any{
		"round_id": "round-a", "participant_actor_ids": []string{"reviewer-a", "reviewer-b"},
		"source_artifact_version_id": versionID,
	}
	roundHeaders := map[string]string{
		"Idempotency-Key": "review-round", "X-Agent-Project": "write-demo",
		"X-Agent-Scopes": "workflow_run.start artifact.write",
	}
	deniedRound := artifactWriteRequest(handler, http.MethodPost, roundPath, roundBody, map[string]string{
		"Idempotency-Key": "review-round-denied", "X-Agent-Project": "write-demo", "X-Agent-Scopes": "workflow_run.start",
	})
	if deniedRound.Code != http.StatusForbidden || !strings.Contains(deniedRound.Body.String(), "artifact.write") {
		t.Fatalf("round missing scope status=%d body=%s", deniedRound.Code, deniedRound.Body.String())
	}
	firstRound := artifactWriteRequest(handler, http.MethodPost, roundPath, roundBody, roundHeaders)
	if firstRound.Code != http.StatusCreated {
		t.Fatalf("design-review round status=%d body=%s", firstRound.Code, firstRound.Body.String())
	}
	firstValue := artifactWriteJSON(t, firstRound)
	firstData := firstValue["data"].(map[string]any)
	workflow := firstData["workflow_run"].(map[string]any)
	updatedArtifact := firstData["artifact"].(map[string]any)
	workflowID := workflow["workflow_run_id"].(string)
	if workflow["workflow_kind"] != "design_review_round" || workflow["phase"] != "pass_1" || workflow["round_id"] != "round-a" ||
		workflow["source_artifact_version_id"] != versionID || workflow["is_resumable"] != false || workflow["state"] != "started" ||
		!equalJSONStrings(workflow["participant_actor_ids"], []string{"reviewer-a", "reviewer-b"}) {
		t.Fatalf("workflow round does not match source contract: %#v", workflow)
	}
	if updatedArtifact["artifact_id"] != artifactID || updatedArtifact["lifecycle_state"] != "active" || updatedArtifact["review_state"] != "collecting_reviews" {
		t.Fatalf("round did not return activated collecting artifact: %#v", updatedArtifact)
	}
	roundProvenance := firstValue["provenance"].(map[string]any)
	if roundProvenance["replay"] != false || roundProvenance["generated_resources"].(map[string]any)["workflow_run_id"] != workflowID {
		t.Fatalf("round provenance=%#v", roundProvenance)
	}

	replay := artifactWriteRequest(handler, http.MethodPost, roundPath, roundBody, roundHeaders)
	replayValue := artifactWriteJSON(t, replay)
	replayData := replayValue["data"].(map[string]any)
	if replay.Code != http.StatusOK || replayValue["provenance"].(map[string]any)["replay"] != true ||
		replayData["workflow_run"].(map[string]any)["workflow_run_id"] != workflowID ||
		replayData["artifact"].(map[string]any)["review_state"] != "collecting_reviews" {
		t.Fatalf("round replay changed stable workflow/artifact output: status=%d body=%s", replay.Code, replay.Body.String())
	}
	runs, err := store.WorkflowRuns(context.Background(), "write-demo", artifactID)
	if err != nil || len(runs) != 1 || runs[0].WorkflowRunID != workflowID {
		t.Fatalf("round replay created duplicate workflow runs: %+v err=%v", runs, err)
	}
	if got, err := store.Artifact(context.Background(), "write-demo", artifactID); err != nil || got == nil || got.ReviewState != "collecting_reviews" {
		t.Fatalf("round artifact state not persisted: %+v err=%v", got, err)
	}

	contributionPath := roundPath + "/" + workflowID + "/contributions"
	contributionHeaders := func(key string) map[string]string {
		return map[string]string{
			"Idempotency-Key": key, "X-Agent-Project": "write-demo", "X-Agent-Scopes": "contribution.write",
		}
	}
	invalidPass2 := artifactWriteRequest(handler, http.MethodPost, contributionPath, map[string]any{
		"phase": "pass_2", "body": "missing read dependency",
	}, contributionHeaders("review-pass2-invalid"))
	if invalidPass2.Code != http.StatusBadRequest || artifactWriteJSON(t, invalidPass2)["error"] != "pass_2_read_set_contributions_required" {
		t.Fatalf("pass_2 without contribution dependency status=%d body=%s", invalidPass2.Code, invalidPass2.Body.String())
	}
	invalidPhase := artifactWriteRequest(handler, http.MethodPost, contributionPath, map[string]any{
		"phase": "synthesis", "body": "wrong route phase",
	}, contributionHeaders("review-phase-invalid"))
	if invalidPhase.Code != http.StatusBadRequest || artifactWriteJSON(t, invalidPhase)["error"] != "invalid_review_phase" {
		t.Fatalf("invalid review phase status=%d body=%s", invalidPhase.Code, invalidPhase.Body.String())
	}

	pass1Body := map[string]any{"phase": "pass_1", "body": "  pass one review  ", "actor_display_name": "Reviewer One"}
	pass1Headers := contributionHeaders("review-pass1")
	pass1 := artifactWriteRequest(handler, http.MethodPost, contributionPath, pass1Body, pass1Headers)
	if pass1.Code != http.StatusCreated {
		t.Fatalf("pass_1 contribution status=%d body=%s", pass1.Code, pass1.Body.String())
	}
	pass1Value := artifactWriteJSON(t, pass1)
	pass1Record := pass1Value["data"].(map[string]any)
	pass1ID := pass1Record["contribution_id"].(string)
	pass1Actor := pass1Value["provenance"].(map[string]any)["actor"].(map[string]any)
	if pass1Record["target_kind"] != "artifact_version" || pass1Record["target_id"] != versionID ||
		pass1Record["contribution_kind"] != "review" || pass1Record["phase"] != "pass_1" || pass1Record["role"] != "reviewer" ||
		pass1Record["body"] != "pass one review" || pass1Record["workflow_run_id"] != workflowID ||
		pass1Record["actor_id"] != pass1Actor["actor_id"] || pass1Actor["display_name"] != "Reviewer One" {
		t.Fatalf("pass_1 contribution metadata/attribution mismatch: %#v", pass1Record)
	}
	if !strings.Contains(string(mustDesignReviewJSON(pass1Record["read_set"])), versionID) {
		t.Fatalf("pass_1 default read set omitted reviewed version: %#v", pass1Record["read_set"])
	}
	pass1Replay := artifactWriteRequest(handler, http.MethodPost, contributionPath, pass1Body, pass1Headers)
	pass1ReplayValue := artifactWriteJSON(t, pass1Replay)
	if pass1Replay.Code != http.StatusOK || pass1ReplayValue["provenance"].(map[string]any)["replay"] != true ||
		pass1ReplayValue["data"].(map[string]any)["contribution_id"] != pass1ID {
		t.Fatalf("pass_1 replay changed contribution id: status=%d body=%s", pass1Replay.Code, pass1Replay.Body.String())
	}

	pass2ReadSet := map[string]any{"versions": []string{versionID}, "contributions": []string{pass1ID}}
	pass2 := artifactWriteRequest(handler, http.MethodPost, contributionPath, map[string]any{
		"phase": "pass_2", "read_set": pass2ReadSet, "body": "pass two review",
	}, contributionHeaders("review-pass2"))
	if pass2.Code != http.StatusCreated {
		t.Fatalf("pass_2 contribution status=%d body=%s", pass2.Code, pass2.Body.String())
	}
	pass2ID := artifactWriteJSON(t, pass2)["data"].(map[string]any)["contribution_id"].(string)

	filters := url.Values{
		"round_id": {"round-a"}, "phase": {"pass_2"}, "role": {"reviewer"},
		"reviewed_version_id": {versionID}, "read_set_contains": {pass1ID},
	}
	listPath := "/v1/projects/write-demo/design-reviews/" + artifactID + "/contributions?" + filters.Encode()
	list := artifactWriteRequest(handler, http.MethodGet, listPath, nil, map[string]string{
		"X-Agent-Project": "write-demo", "X-Agent-Scopes": "artifact.read",
	})
	if list.Code != http.StatusOK {
		t.Fatalf("filtered review list status=%d body=%s", list.Code, list.Body.String())
	}
	listValue := artifactWriteJSON(t, list)
	listData := listValue["data"].([]any)
	if len(listData) != 1 || listData[0].(map[string]any)["contribution_id"] != pass2ID || listValue["chunking_status"].(map[string]any)["status"] != "none" {
		t.Fatalf("review filters returned unexpected entries: %#v", listValue)
	}

	workflowAfterContributions, err := store.WorkflowRun(context.Background(), workflowID)
	if err != nil || workflowAfterContributions == nil || !equalStrings(workflowAfterContributions.GeneratedContributionIDs, []string{pass1ID, pass2ID}) {
		t.Fatalf("review workflow output IDs missing or duplicated: %+v err=%v", workflowAfterContributions, err)
	}

	synthesisPath := roundPath + "/" + workflowID + "/synthesis"
	synthesisHeaders := map[string]string{
		"Idempotency-Key": "review-synthesis", "X-Agent-Project": "write-demo",
		"X-Agent-Scopes": "contribution.write artifact_version.create artifact.write",
	}
	synthesisBody := map[string]any{
		"read_set": map[string]any{"contributions": []string{pass2ID}}, "body": "  # Synthesized design  ",
		"version_label": "synthesis", "actor_display_name": "Synthesis Writer",
	}
	missingSynthesisScope := artifactWriteRequest(handler, http.MethodPost, synthesisPath, synthesisBody, map[string]string{
		"Idempotency-Key": "review-synthesis-denied", "X-Agent-Project": "write-demo", "X-Agent-Scopes": "contribution.write artifact.write",
	})
	if missingSynthesisScope.Code != http.StatusForbidden || !strings.Contains(missingSynthesisScope.Body.String(), "artifact_version.create") {
		t.Fatalf("synthesis missing scope status=%d body=%s", missingSynthesisScope.Code, missingSynthesisScope.Body.String())
	}
	invalidSynthesisReadSet := artifactWriteRequest(handler, http.MethodPost, synthesisPath, map[string]any{
		"read_set": map[string]any{"versions": []string{versionID}}, "body": "missing contribution dependency",
	}, map[string]string{
		"Idempotency-Key": "review-synthesis-invalid-read-set", "X-Agent-Project": "write-demo", "X-Agent-Scopes": synthesisHeaders["X-Agent-Scopes"],
	})
	if invalidSynthesisReadSet.Code != http.StatusBadRequest || artifactWriteJSON(t, invalidSynthesisReadSet)["error"] != "synthesis_read_set_contributions_required" {
		t.Fatalf("synthesis read-set admission status=%d body=%s", invalidSynthesisReadSet.Code, invalidSynthesisReadSet.Body.String())
	}
	synthesis := artifactWriteRequest(handler, http.MethodPost, synthesisPath, synthesisBody, synthesisHeaders)
	if synthesis.Code != http.StatusCreated {
		t.Fatalf("design-review synthesis status=%d body=%s", synthesis.Code, synthesis.Body.String())
	}
	synthesisValue := artifactWriteJSON(t, synthesis)
	synthesisData := synthesisValue["data"].(map[string]any)
	synthesisContribution := synthesisData["contribution"].(map[string]any)
	synthesisVersion := synthesisData["version"].(map[string]any)
	synthesisContributionID := synthesisContribution["contribution_id"].(string)
	synthesisVersionID := synthesisVersion["artifact_version_id"].(string)
	if synthesisContribution["contribution_kind"] != "synthesis" || synthesisContribution["phase"] != "synthesis" ||
		synthesisContribution["role"] != "analyst" || synthesisContribution["target_id"] != versionID ||
		synthesisContribution["body"] != "# Synthesized design" || synthesisData["artifact"].(map[string]any)["review_state"] != "needs_user_decision" {
		t.Fatalf("synthesis response does not match source contract: %#v", synthesisData)
	}
	synthesisReplay := artifactWriteRequest(handler, http.MethodPost, synthesisPath, synthesisBody, synthesisHeaders)
	synthesisReplayValue := artifactWriteJSON(t, synthesisReplay)
	synthesisReplayData := synthesisReplayValue["data"].(map[string]any)
	if synthesisReplay.Code != http.StatusOK || synthesisReplayValue["provenance"].(map[string]any)["replay"] != true ||
		synthesisReplayData["contribution"].(map[string]any)["contribution_id"] != synthesisContributionID ||
		synthesisReplayData["version"].(map[string]any)["artifact_version_id"] != synthesisVersionID {
		t.Fatalf("synthesis replay changed stable output IDs: status=%d body=%s", synthesisReplay.Code, synthesisReplay.Body.String())
	}
	withoutVersionBody := map[string]any{"read_set": map[string]any{"contribution": []string{pass2ID}}, "body": "Synthesis without version", "create_version": false}
	withoutVersion := artifactWriteRequest(handler, http.MethodPost, synthesisPath, withoutVersionBody, map[string]string{
		"Idempotency-Key": "review-synthesis-no-version", "X-Agent-Project": "write-demo", "X-Agent-Scopes": synthesisHeaders["X-Agent-Scopes"],
	})
	if withoutVersion.Code != http.StatusCreated {
		t.Fatalf("synthesis without version status=%d body=%s", withoutVersion.Code, withoutVersion.Body.String())
	}
	withoutVersionData := artifactWriteJSON(t, withoutVersion)["data"].(map[string]any)
	if withoutVersionData["version"] != nil || withoutVersionData["artifact"].(map[string]any)["review_state"] != "needs_user_decision" {
		t.Fatalf("optional synthesis version/state mismatch: %#v", withoutVersionData)
	}
	workflowAfterSynthesis, err := store.WorkflowRun(context.Background(), workflowID)
	if err != nil || workflowAfterSynthesis == nil ||
		!equalStrings(workflowAfterSynthesis.GeneratedContributionIDs, []string{pass1ID, pass2ID, synthesisContributionID, withoutVersionData["contribution"].(map[string]any)["contribution_id"].(string)}) ||
		!equalStrings(workflowAfterSynthesis.GeneratedVersionIDs, []string{synthesisVersionID}) {
		t.Fatalf("synthesis workflow outputs mismatch: %+v err=%v", workflowAfterSynthesis, err)
	}

	actorID := createProvenance["actor"].(map[string]any)["actor_id"].(string)
	otherArtifact, err := store.CreateArtifact(context.Background(), storage.ArtifactInsert{
		ProjectIdent: "write-demo", Kind: "design_review", Title: "Other review", CreatedByActorID: actorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongRound := artifactWriteRequest(handler, http.MethodPost,
		"/v1/projects/write-demo/design-reviews/"+otherArtifact.ArtifactID+"/rounds/"+workflowID+"/contributions",
		map[string]any{"phase": "pass_1", "body": "wrong artifact round"}, contributionHeaders("wrong-review-round"))
	if wrongRound.Code != http.StatusBadRequest || !strings.Contains(wrongRound.Body.String(), "workflow_run does not belong to artifact") {
		t.Fatalf("cross-artifact review round status=%d body=%s", wrongRound.Code, wrongRound.Body.String())
	}
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "review-other-project", ChannelName: "review-other-project", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	crossProjectList := artifactWriteRequest(handler, http.MethodGet,
		"/v1/projects/review-other-project/design-reviews/"+artifactID+"/contributions", nil,
		map[string]string{"X-Agent-Project": "review-other-project", "X-Agent-Scopes": "artifact.read"})
	if crossProjectList.Code != http.StatusNotFound {
		t.Fatalf("cross-project design-review listing status=%d body=%s", crossProjectList.Code, crossProjectList.Body.String())
	}
}

func mustDesignReviewJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func equalJSONStrings(value any, want []string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for i := range want {
		if items[i] != want[i] {
			return false
		}
	}
	return true
}
