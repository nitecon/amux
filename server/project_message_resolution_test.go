package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func TestProjectMessageResolutionRoutesMatchRustScopeAndResponses(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, storage.Project{Ident: "resolution", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "other", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	oldRoot, err := store.CreateHumanProjectMessage(ctx, "resolution", storage.HumanProjectMessageInput{Content: "old root"})
	if err != nil {
		t.Fatal(err)
	}
	for time.Now().UTC().UnixMilli() <= oldRoot.SentAt {
		time.Sleep(time.Millisecond)
	}
	boundaryRoot, err := store.CreateHumanProjectMessage(ctx, "resolution", storage.HumanProjectMessageInput{Content: "boundary root"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := store.CreateHumanProjectMessage(ctx, "resolution", storage.HumanProjectMessageInput{Content: "reply", ParentMessageID: &boundaryRoot.ID})
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot, err := store.CreateHumanProjectMessage(ctx, "other", storage.HumanProjectMessageInput{Content: "foreign root"})
	if err != nil {
		t.Fatal(err)
	}
	agentRoot, err := store.CreateAgentProjectMessage(ctx, "resolution", *apiActor(), storage.AgentProjectMessageInput{Content: "agent root", MessageType: "message"})
	if err != nil {
		t.Fatal(err)
	}

	bulkPath := "/v1/projects/resolution/messages/resolve"
	body := fmt.Sprintf(`{"before_ms":%d,"author_kind":" HUMAN ","author_kinds":["human"," "]}`, boundaryRoot.SentAt)
	resolved := taskRequest(handler, http.MethodPost, bulkPath, body, apiActor())
	if resolved.Code != http.StatusOK || strings.TrimSpace(resolved.Body.String()) != `{"resolved":1}` {
		t.Fatalf("bulk strict-cutoff response status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	assertMessageResolution(t, store, "resolution", oldRoot.ID, true, "agent:worker")
	assertMessageResolution(t, store, "resolution", boundaryRoot.ID, false, "")
	assertMessageResolution(t, store, "resolution", reply.ID, false, "")
	assertMessageResolution(t, store, "resolution", agentRoot.ID, false, "")
	assertMessageResolution(t, store, "other", foreignRoot.ID, false, "")
	repeatedBulk := taskRequest(handler, http.MethodPost, bulkPath, body, apiActor())
	if repeatedBulk.Code != http.StatusOK || strings.TrimSpace(repeatedBulk.Body.String()) != `{"resolved":0}` {
		t.Fatalf("repeated bulk response status=%d body=%s", repeatedBulk.Code, repeatedBulk.Body.String())
	}

	replyPath := "/v1/projects/resolution/messages/" + strconv.FormatInt(reply.ID, 10)
	resolvedReply := taskRequest(handler, http.MethodPost, replyPath+"/resolve", "", apiActor())
	if resolvedReply.Code != http.StatusOK || strings.TrimSpace(resolvedReply.Body.String()) != `{"changed":true}` {
		t.Fatalf("resolve reply response status=%d body=%s", resolvedReply.Code, resolvedReply.Body.String())
	}
	assertMessageResolution(t, store, "resolution", reply.ID, true, "agent:worker")
	resolvedAgain := taskRequest(handler, http.MethodPost, replyPath+"/resolve", "", apiActor())
	if resolvedAgain.Code != http.StatusOK || strings.TrimSpace(resolvedAgain.Body.String()) != `{"changed":false}` {
		t.Fatalf("repeated resolve response status=%d body=%s", resolvedAgain.Code, resolvedAgain.Body.String())
	}
	foreignResolve := taskRequest(handler, http.MethodPost, "/v1/projects/resolution/messages/"+strconv.FormatInt(foreignRoot.ID, 10)+"/resolve", "", apiActor())
	if foreignResolve.Code != http.StatusOK || strings.TrimSpace(foreignResolve.Body.String()) != `{"changed":false}` {
		t.Fatalf("foreign resolve response status=%d body=%s", foreignResolve.Code, foreignResolve.Body.String())
	}
	missingResolve := taskRequest(handler, http.MethodPost, "/v1/projects/resolution/messages/9223372036854775807/resolve", "", apiActor())
	if missingResolve.Code != http.StatusOK || strings.TrimSpace(missingResolve.Body.String()) != `{"changed":false}` {
		t.Fatalf("missing resolve response status=%d body=%s", missingResolve.Code, missingResolve.Body.String())
	}
	reopened := taskRequest(handler, http.MethodPost, replyPath+"/reopen", "", apiActor())
	if reopened.Code != http.StatusOK || strings.TrimSpace(reopened.Body.String()) != `{"changed":true}` {
		t.Fatalf("reopen reply response status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	assertMessageResolution(t, store, "resolution", reply.ID, false, "")
	reopenedAgain := taskRequest(handler, http.MethodPost, replyPath+"/reopen", "", apiActor())
	if reopenedAgain.Code != http.StatusOK || strings.TrimSpace(reopenedAgain.Body.String()) != `{"changed":false}` {
		t.Fatalf("repeated reopen response status=%d body=%s", reopenedAgain.Code, reopenedAgain.Body.String())
	}
	foreignReopen := taskRequest(handler, http.MethodPost, "/v1/projects/resolution/messages/"+strconv.FormatInt(foreignRoot.ID, 10)+"/reopen", "", apiActor())
	if foreignReopen.Code != http.StatusOK || strings.TrimSpace(foreignReopen.Body.String()) != `{"changed":false}` {
		t.Fatalf("foreign reopen response status=%d body=%s", foreignReopen.Code, foreignReopen.Body.String())
	}

	for time.Now().UTC().UnixMilli() <= agentRoot.SentAt {
		time.Sleep(time.Millisecond)
	}
	defaultBulk := taskRequest(handler, http.MethodPost, bulkPath, "", nil)
	if defaultBulk.Code != http.StatusOK || strings.TrimSpace(defaultBulk.Body.String()) != `{"resolved":2}` {
		t.Fatalf("bodyless bulk response status=%d body=%s", defaultBulk.Code, defaultBulk.Body.String())
	}
}

func assertMessageResolution(t *testing.T, store *storage.Store, project string, id int64, resolved bool, by string) {
	t.Helper()
	message, err := store.ProjectMessage(context.Background(), project, id)
	if err != nil || message == nil {
		t.Fatalf("load message %d in %s: %+v err=%v", id, project, message, err)
	}
	if (message.ResolvedAt != nil) != resolved {
		t.Fatalf("message %d resolved_at=%v, want resolved=%t", id, message.ResolvedAt, resolved)
	}
	gotBy := ""
	if message.ResolvedBy != nil {
		gotBy = *message.ResolvedBy
	}
	if gotBy != by {
		t.Fatalf("message %d resolved_by=%q, want %q", id, gotBy, by)
	}
}
