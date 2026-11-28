package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestProjectMessageThreadTaskToolIsScopedReadOnlyAndReturnsFullText(t *testing.T) {
	ctx := context.Background()
	store, _ := taskAPI(t)
	actor := apiActor()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Thread reader", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Read a project message")}, actor)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("original-full-message-", 4000) + "tail-marker-after-inbox-ack"
	message, err := store.CreateHumanProjectMessage(ctx, "tasks", storage.HumanProjectMessageInput{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed, err := store.ConfirmProjectMessage(ctx, "tasks", actor.SessionID, message.ID); err != nil || !confirmed {
		t.Fatalf("inbox confirmation=%t err=%v", confirmed, err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "other", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.CreateHumanProjectMessage(ctx, "other", storage.HumanProjectMessageInput{Content: "foreign project thread"})
	if err != nil {
		t.Fatal(err)
	}
	threadBefore, err := store.GetProjectMessageThread(ctx, "tasks", message.ID)
	if err != nil {
		t.Fatal(err)
	}

	invoke := func(origin *agent.Origin, args string) storage.ToolResult {
		t.Helper()
		w := invokeTaskTool(t, newAPI(store, "unused"), origin, conversation.ID, `{"name":"project_message_thread","arguments":`+args+`}`)
		if w.Code != 200 {
			t.Fatalf("tool HTTP %d: %s", w.Code, w.Body.String())
		}
		var result storage.ToolResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := invoke(actor, fmt.Sprintf(`{"id":%d}`, message.ID))
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("thread result=%+v", result)
	}
	var thread storage.ProjectMessageThread
	if err := json.Unmarshal([]byte(result.Content[0].Text), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Root.ID != message.ID || thread.Root.Content != content || !strings.HasSuffix(thread.Root.Content, "tail-marker-after-inbox-ack") || thread.State != "acknowledged" || len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != actor.SessionID {
		t.Fatalf("thread tool returned truncated or inconsistent original: rootbytes=%d state=%q confirmations=%+v", len(thread.Root.Content), thread.State, thread.Confirmations)
	}
	if bad := invoke(actor, fmt.Sprintf(`{"id":%d,"project":"other"}`, message.ID)); !bad.IsError || !strings.Contains(bad.Content[0].Text, "unknown argument") {
		t.Fatalf("caller-controlled project was accepted: %+v", bad)
	}
	if bad := invoke(actor, fmt.Sprintf(`{"id":%d}`, foreign.ID)); !bad.IsError || !strings.Contains(bad.Content[0].Text, "not found in this project") {
		t.Fatalf("foreign message was exposed: %+v", bad)
	}
	if bad := invoke(actor, `{"id":0}`); !bad.IsError {
		t.Fatalf("non-positive message ID was accepted: %+v", bad)
	}
	stale := *actor
	stale.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	w := invokeTaskTool(t, newAPI(store, "unused"), &stale, conversation.ID, `{"name":"project_message_thread","arguments":{"id":1}}`)
	if w.Code == 200 {
		t.Fatalf("stale conversation owner was accepted: %s", w.Body.String())
	}
	threadAfter, err := store.GetProjectMessageThread(ctx, "tasks", message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(threadBefore, threadAfter) {
		t.Fatalf("read tool mutated thread/confirmations: before=%+v after=%+v", threadBefore, threadAfter)
	}
}
