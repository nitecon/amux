package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func TestNativeTaskCommentsPreserveRESTContentLimits(t *testing.T) {
	s, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Comment validation", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(s, "unused")
	for _, body := range []string{"", " \n\t", strings.Repeat("x", 65537)} {
		raw, _ := json.Marshal(map[string]any{"name": "task_comment", "arguments": map[string]any{"id": task.ID, "body": body}})
		w := invokeTaskTool(t, a, actor, c.ID, string(raw))
		var result mcpstdio.Result
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.IsError {
			t.Fatalf("invalid comment accepted: status=%d error=%v", w.Code, err)
		}
	}
	loaded, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || len(loaded.Comments) != 0 {
		t.Fatalf("invalid comments persisted: %+v %v", loaded, err)
	}
}
