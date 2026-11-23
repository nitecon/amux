package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestProjectMessageRoomReturnsInternalProjectAndRejectsUnavailableExternal(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "native", ChannelName: "internal", RoomID: "native", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	internal := taskRequest(handler, http.MethodPost, "/v1/projects/native/room", "", nil)
	if internal.Code != http.StatusOK {
		t.Fatalf("internal room status=%d body=%s", internal.Code, internal.Body.String())
	}
	var project storage.Project
	if err := json.Unmarshal(internal.Body.Bytes(), &project); err != nil || project.Ident != "native" || project.ChannelName != "internal" {
		t.Fatalf("internal room project=%+v err=%v", project, err)
	}
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "external", ChannelName: "discord", RoomID: "room-1", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	external := taskRequest(handler, http.MethodPost, "/v1/projects/external/room", "", nil)
	if external.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured external room status=%d body=%s", external.Code, external.Body.String())
	}
	missing := taskRequest(handler, http.MethodPost, "/v1/projects/missing/room", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing room project status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestProjectRegistrationRejectsUnknownExternalChannelWithoutMutation(t *testing.T) {
	store, handler := taskAPI(t)
	response := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"new-project","channel":"discord","repo_url":"https://github.com/example/new-project.git"}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("registration status=%d body=%s", response.Code, response.Body.String())
	}
	project, err := store.Project(context.Background(), "new-project")
	if err != nil || project != nil {
		t.Fatalf("unknown channel registration mutated storage: project=%+v err=%v", project, err)
	}
}

func TestAgentProjectMessagesPreserveBodyOriginDefaultsAndThreadReceipts(t *testing.T) {
	store, handler := taskAPI(t)
	for _, request := range []struct {
		body string
		kind string
	}{
		{body: `{"ident":"native"}`, kind: "adhoc"},
		{body: `{"ident":"repo","channel":"internal","repo_url":"https://github.com/example/repo.git"}`, kind: "repo"},
	} {
		response := taskRequest(handler, http.MethodPost, "/v1/projects", request.body, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("register project status=%d body=%s", response.Code, response.Body.String())
		}
		var registered projectRegistration
		if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil || registered.Kind != request.kind || registered.Channel != "internal" || registered.Room != registered.Ident {
			t.Fatalf("registered project=%+v err=%v", registered, err)
		}
	}
	again := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"native"}`, nil)
	if again.Code != http.StatusOK {
		t.Fatalf("repeat register status=%d body=%s", again.Code, again.Body.String())
	}
	var registeredAgain projectRegistration
	if err := json.Unmarshal(again.Body.Bytes(), &registeredAgain); err != nil || registeredAgain.Ident != "native" || registeredAgain.Room != "native" || registeredAgain.Channel != "internal" || registeredAgain.Kind != "adhoc" {
		t.Fatalf("repeat registration changed native project: %+v err=%v", registeredAgain, err)
	}
	room := taskRequest(handler, http.MethodPost, "/v1/projects/native/room", "", nil)
	if room.Code != http.StatusOK {
		t.Fatalf("native room status=%d body=%s", room.Code, room.Body.String())
	}
	nullRequest := taskRequest(handler, http.MethodPost, "/v1/projects/missing/messages", "null", apiActor())
	if nullRequest.Code != http.StatusBadRequest {
		t.Fatalf("null request status=%d body=%s", nullRequest.Code, nullRequest.Body.String())
	}
	origin := apiActor()
	actor := *origin
	rootBody := "  first line  \n second line "
	root := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"`+strings.ReplaceAll(rootBody, "\n", `\n`)+`","content":"alias","event_at":0}`, origin)
	if root.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", root.Code, root.Body.String())
	}
	var sent agentProjectMessageReceipt
	if err := json.Unmarshal(root.Body.Bytes(), &sent); err != nil || sent.MessageID <= 0 || sent.ParentMessageID != nil || sent.ExternalMessageID != "internal:"+strconv.FormatInt(sent.MessageID, 10) {
		t.Fatalf("send receipt=%+v err=%v", sent, err)
	}
	thread, err := store.GetProjectMessageThread(context.Background(), "native", sent.MessageID)
	if err != nil || thread.Root.Content != rootBody || thread.Root.AgentID == nil || *thread.Root.AgentID != actor.SessionID || thread.Root.Subject == nil || *thread.Root.Subject != "first line" || thread.Root.Hostname == nil || *thread.Root.Hostname != actor.SessionID || thread.Root.EventAt == nil || *thread.Root.EventAt != 0 || thread.Root.ConfirmedAt == nil {
		t.Fatalf("stored root=%+v err=%v", thread, err)
	}
	foreignReply := taskRequest(handler, http.MethodPost, "/v1/projects/repo/messages/"+strconv.FormatInt(sent.MessageID, 10)+"/reply", `{"body":"foreign"}`, origin)
	if foreignReply.Code != http.StatusNotFound {
		t.Fatalf("cross-project reply status=%d body=%s", foreignReply.Code, foreignReply.Body.String())
	}
	foreignList, err := store.ListProjectMessageThreads(context.Background(), "repo", storage.ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || len(foreignList.Threads) != 0 {
		t.Fatalf("cross-project reply mutated target: %+v err=%v", foreignList, err)
	}

	blankBody := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"","content":"must not win"}`, origin)
	if blankBody.Code != http.StatusBadRequest {
		t.Fatalf("empty body precedence status=%d body=%s", blankBody.Code, blankBody.Body.String())
	}
	spoof := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"x","agent_id":"spoof"}`, origin)
	if spoof.Code != http.StatusOK {
		t.Fatalf("unknown sender field status=%d body=%s", spoof.Code, spoof.Body.String())
	}
	var spoofReceipt agentProjectMessageReceipt
	if err := json.Unmarshal(spoof.Body.Bytes(), &spoofReceipt); err != nil {
		t.Fatal(err)
	}
	spoofThread, err := store.GetProjectMessageThread(context.Background(), "native", spoofReceipt.MessageID)
	if err != nil || spoofThread.Root.AgentID == nil || *spoofThread.Root.AgentID != actor.SessionID {
		t.Fatalf("request field changed authenticated sender: thread=%+v err=%v", spoofThread, err)
	}
	noOrigin := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"no sender"}`, nil)
	if noOrigin.Code != http.StatusBadRequest {
		t.Fatalf("missing native origin status=%d body=%s", noOrigin.Code, noOrigin.Body.String())
	}
	partial := httptestNewTaskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"partial"}`, map[string]string{"X-Agent-Session-Id": actor.SessionID})
	if partial.Code != http.StatusBadRequest {
		t.Fatalf("partial native origin status=%d body=%s", partial.Code, partial.Body.String())
	}

	reply := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages/"+strconv.FormatInt(sent.MessageID, 10)+"/reply", `{"content":"reply","subject":"","hostname":" ","event_at":9}`, origin)
	if reply.Code != http.StatusOK {
		t.Fatalf("reply status=%d body=%s", reply.Code, reply.Body.String())
	}
	var replied agentProjectMessageReceipt
	if err := json.Unmarshal(reply.Body.Bytes(), &replied); err != nil || replied.MessageID <= sent.MessageID || replied.ParentMessageID == nil || *replied.ParentMessageID != sent.MessageID {
		t.Fatalf("reply receipt=%+v err=%v", replied, err)
	}
	thread, err = store.GetProjectMessageThread(context.Background(), "native", sent.MessageID)
	if err != nil || len(thread.Replies) != 1 || thread.Replies[0].Content != "reply" || thread.Replies[0].Subject == nil || *thread.Replies[0].Subject != "reply" || thread.Replies[0].Hostname == nil || *thread.Replies[0].Hostname != actor.SessionID || thread.Replies[0].EventAt == nil || *thread.Replies[0].EventAt != 9 {
		t.Fatalf("stored reply thread=%+v err=%v", thread, err)
	}

	action := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages/"+strconv.FormatInt(sent.MessageID, 10)+"/action", `{"message":"action line"}`, origin)
	if action.Code != http.StatusOK {
		t.Fatalf("action status=%d body=%s", action.Code, action.Body.String())
	}
	var acted agentProjectMessageReceipt
	if err := json.Unmarshal(action.Body.Bytes(), &acted); err != nil || acted.ParentMessageID == nil || *acted.ParentMessageID != sent.MessageID {
		t.Fatalf("action receipt=%+v err=%v", acted, err)
	}
	thread, err = store.GetProjectMessageThread(context.Background(), "native", sent.MessageID)
	if err != nil || len(thread.Replies) != 2 || thread.Replies[1].MessageType != "action" || thread.Replies[1].Subject == nil || *thread.Replies[1].Subject != "[ACTION] action line" {
		t.Fatalf("stored action thread=%+v err=%v", thread, err)
	}
	plainAction := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages/"+strconv.FormatInt(sent.MessageID, 10)+"/action", `{"message":"plain subject","subject":""}`, origin)
	if plainAction.Code != http.StatusOK {
		t.Fatalf("blank-subject action status=%d body=%s", plainAction.Code, plainAction.Body.String())
	}
	thread, err = store.GetProjectMessageThread(context.Background(), "native", sent.MessageID)
	if err != nil || len(thread.Replies) != 3 || thread.Replies[2].Subject == nil || *thread.Replies[2].Subject != "plain subject" {
		t.Fatalf("blank action subject was prefixed: thread=%+v err=%v", thread, err)
	}
	ignoredRootAlias := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"root","message":{"debug":1}}`, origin)
	if ignoredRootAlias.Code != http.StatusOK {
		t.Fatalf("unused root alias status=%d body=%s", ignoredRootAlias.Code, ignoredRootAlias.Body.String())
	}
	ignoredActionAlias := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages/"+strconv.FormatInt(sent.MessageID, 10)+"/action", `{"body":"action","content":123}`, origin)
	if ignoredActionAlias.Code != http.StatusOK {
		t.Fatalf("unused action alias status=%d body=%s", ignoredActionAlias.Code, ignoredActionAlias.Body.String())
	}
	invalidSelectedAlias := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", `{"body":"body wins","content":123}`, origin)
	if invalidSelectedAlias.Code != http.StatusBadRequest {
		t.Fatalf("wrong-type selected alias status=%d body=%s", invalidSelectedAlias.Code, invalidSelectedAlias.Body.String())
	}
}

func TestUnavailableAgentMessageChannelsDoNotStoreAndParentIsScoped(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, storage.Project{Ident: "external", ChannelName: "discord", RoomID: "room", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "other", ChannelName: "internal", RoomID: "other", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	foreignParent, err := store.CreateHumanProjectMessage(ctx, "other", storage.HumanProjectMessageInput{Content: "foreign root"})
	if err != nil {
		t.Fatal(err)
	}
	origin := apiActor()
	sent := taskRequest(handler, http.MethodPost, "/v1/projects/external/messages", `{"body":"external send"}`, origin)
	if sent.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured external send status=%d body=%s", sent.Code, sent.Body.String())
	}
	crossProject := taskRequest(handler, http.MethodPost, "/v1/projects/external/messages/"+strconv.FormatInt(foreignParent.ID, 10)+"/reply", `{"body":"reply"}`, origin)
	if crossProject.Code != http.StatusNotFound {
		t.Fatalf("cross-project parent status=%d body=%s", crossProject.Code, crossProject.Body.String())
	}
	list, err := store.ListProjectMessageThreads(ctx, "external", storage.ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || len(list.Threads) != 0 || list.Counts != (storage.ProjectMessageInboxCounts{}) {
		t.Fatalf("unavailable channel stored agent messages: list=%+v err=%v", list, err)
	}
}

func httptestNewTaskRequest(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer task-key")
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
