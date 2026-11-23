package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderNativeNotificationIncludesTaskAndProvenance(t *testing.T) {
	payload := json.RawMessage(`{"target":"receiver","project":"proj","mode":"recipient","require_idle":false,"heading":"Task changed","instructions":"Inspect the update.","event":{"kind":"task_commented","sender_name":"Alice","sender_hostname":"Sender-Host","owner_hostname":"Owner-Host","comment":{"author":"Bob","content":"A useful comment."},"task":{"id":"task-123","title":"Review title","status":"in_progress","hostname":"Creation-Host","description":"Task description.","specification":"Review specification.","details":"Extra details."}}}`)
	got, err := renderNativeNotification(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[amux notification]", "Heading: Task changed", "Project: proj", "Task ID: task-123",
		"Title: Review title", "Status: in_progress", "Sender: Alice",
		"Sender host snapshot: Sender-Host", "Owner host snapshot: Owner-Host",
		"Task host snapshot: Creation-Host", "Comment author: Bob", "Comment:\nA useful comment.",
		"Description:\nTask description.", "Specification:\nReview specification.",
		"Details:\nExtra details.", "Instructions:\nInspect the update.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, got)
		}
	}
}

func TestRenderNativeNotificationPreservesAssignedAndReviewInstructions(t *testing.T) {
	tests := []struct {
		name, payload, instruction string
	}{
		{
			name:        "assigned reminder",
			payload:     `{"target":"s","project":"p","mode":"assigned","require_idle":true,"reminder":true,"heading":"Task update requested","instructions":"You still own task task-9. Post a progress update.","event":{"kind":"task_created","task":{"id":"task-9","title":"Work","status":"in_progress"}}}`,
			instruction: "You still own task task-9. Post a progress update.",
		},
		{
			name:        "review only",
			payload:     `{"target":"s","project":"p","mode":"assigned","require_idle":true,"heading":"Queued independent review","instructions":"Review only: do not edit code or implement findings. Record confirmed defects.","event":{"kind":"task_created","task":{"id":"review-1","title":"Review","status":"in_progress"}}}`,
			instruction: "Review only: do not edit code or implement findings. Record confirmed defects.",
		},
		{
			name:        "previous execution information",
			payload:     `{"target":"s","project":"p","mode":"recipient","require_idle":true,"receiver":{"session_id":"s","instance_id":"i","project":"p"},"heading":"In-progress task reminder","instructions":"This conversation's previous execution still owns this task. This is an informational continuity reminder, not an ownership handoff.","event":{"kind":"task_created","task":{"id":"old-1","title":"Prior work","status":"in_progress"}}}`,
			instruction: "This conversation's previous execution still owns this task. This is an informational continuity reminder, not an ownership handoff.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderNativeNotification(json.RawMessage(tt.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, "Instructions:\n"+tt.instruction) {
				t.Fatalf("supplied instructions not preserved:\n%s", got)
			}
			if strings.Contains(strings.ToLower(got), "execute this task") || strings.Contains(strings.ToLower(got), "start executing") {
				t.Fatalf("formatter invented execution instructions:\n%s", got)
			}
		})
	}
}

func TestRenderNativeNotificationUsesAsStringForOptionalFields(t *testing.T) {
	payload := json.RawMessage(`{"target":"s","project":"p","mode":"assigned","require_idle":true,"heading":{"unexpected":true},"instructions":17,"event":{"kind":"task_created","task":{"id":"task-1"}}}`)
	got, err := renderNativeNotification(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Heading:") || strings.Contains(got, "Instructions:") {
		t.Fatalf("wrong-typed optional display fields were not ignored:\n%s", got)
	}
}

func TestRenderNativeNotificationBoundsUnicodeBodyWithoutLosingIdentity(t *testing.T) {
	longText := strings.Repeat("界", 25_000)
	payload := json.RawMessage(`{"target":"s","project":"p","mode":"assigned","require_idle":true,"instructions":"Review only: preserve this instruction.","unused_integer":900719925474099312345678901234567890,"event":{"kind":"task_created","task":{"id":"stable-task-id","title":"Large","description":"` + longText + `"}}}`)
	got, err := renderNativeNotification(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > nativeNotificationMaxBytes || !utf8.ValidString(got) {
		t.Fatalf("rendered output bytes=%d validUTF8=%v", len(got), utf8.ValidString(got))
	}
	for _, want := range []string{"Task ID: stable-task-id", "Review only: preserve this instruction.", "read task stable-task-id for the full text"} {
		if !strings.Contains(got, want) {
			t.Errorf("bounded prompt missing %q", want)
		}
	}
	if strings.Contains(got, "900719925474099312345678901234567890") {
		t.Fatal("unused JSON integer was unnecessarily rendered")
	}
}

func TestRenderNativeNotificationRemovesNULAndPreservesBodyNewlines(t *testing.T) {
	payload := json.RawMessage(`{"target":"s","project":"p","mode":"recipient","require_idle":false,"event":{"kind":"task_commented","comment":{"author":"A","content":"first\nsecond\u0000third"},"task":{"id":"task-1"}}}`)
	got, err := renderNativeNotification(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "first\nsecondthird") || strings.ContainsRune(got, '\x00') {
		t.Fatalf("comment was not normalized for a command:\n%q", got)
	}
}

func TestRenderNativeNotificationRejectsMalformedPublicPayload(t *testing.T) {
	for _, payload := range []string{
		`{`, `[]`, `null`, `{"target":"s","project":"p","mode":"assigned","event":{"kind":"task_created","task":{}},"require_idle":true} false`,
		`{"target":"s","project":"p","mode":"invalid","event":{"kind":"task_created","task":{}},"require_idle":true}`,
	} {
		if got, err := renderNativeNotification(json.RawMessage(payload)); err == nil || got != "" {
			t.Errorf("malformed payload returned prompt=%q err=%v", got, err)
		}
	}
}
