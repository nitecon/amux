package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/auth/authn"
	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/storage"
)

type externalMessageCall struct {
	room    string
	parent  *string
	message channels.OutboundMessage
	text    string
}

type fakeOutbound struct {
	mu             sync.Mutex
	rooms          []string
	structured     []externalMessageCall
	textMessages   []externalMessageCall
	nextID         int
	failStructured bool
	failText       bool
}

func (f *fakeOutbound) EnsureRoom(_ context.Context, project string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms = append(f.rooms, project)
	return "room-" + project, nil
}

func (f *fakeOutbound) Send(_ context.Context, room string, parent *string, message channels.OutboundMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.structured = append(f.structured, externalMessageCall{room: room, parent: copyExternalID(parent), message: message})
	if f.failStructured {
		return "", errors.New("structured send failed")
	}
	f.nextID++
	return fmt.Sprintf("provider-%d", f.nextID), nil
}

func (f *fakeOutbound) SendText(_ context.Context, room string, parent *string, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textMessages = append(f.textMessages, externalMessageCall{room: room, parent: copyExternalID(parent), text: text})
	if f.failText {
		return "", errors.New("text send failed")
	}
	f.nextID++
	return fmt.Sprintf("provider-%d", f.nextID), nil
}

func copyExternalID(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (f *fakeOutbound) snapshot() (rooms []string, structured, text []externalMessageCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rooms...), append([]externalMessageCall(nil), f.structured...), append([]externalMessageCall(nil), f.textMessages...)
}

func TestConfiguredExternalProjectMessagesAndLazyAdhocRooms(t *testing.T) {
	store, _ := taskAPI(t)
	provider := &fakeOutbound{}
	handler, err := NewHandlerWithConfig(store, "task-key", HandlerConfig{Channels: map[string]channels.Outbound{"external": provider}})
	if err != nil {
		t.Fatal(err)
	}

	repo := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"repo","channel":"external","repo_url":"https://github.com/example/repo.git"}`, nil)
	if repo.Code != http.StatusOK {
		t.Fatalf("external repository registration status=%d body=%s", repo.Code, repo.Body.String())
	}
	var registered projectRegistration
	if err := json.Unmarshal(repo.Body.Bytes(), &registered); err != nil || registered.Room != "room-repo" || registered.Kind != "repo" {
		t.Fatalf("external repository registration=%+v err=%v", registered, err)
	}
	adhoc := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"adhoc","channel":"external"}`, nil)
	if adhoc.Code != http.StatusOK {
		t.Fatalf("external adhoc registration status=%d body=%s", adhoc.Code, adhoc.Body.String())
	}
	if err := json.Unmarshal(adhoc.Body.Bytes(), &registered); err != nil || registered.Room != "" || registered.Kind != "adhoc" {
		t.Fatalf("lazy external adhoc registration=%+v err=%v", registered, err)
	}
	rooms, _, _ := provider.snapshot()
	if len(rooms) != 1 || rooms[0] != "repo" {
		t.Fatalf("rooms after registration=%v, want repository room only", rooms)
	}
	room := taskRequest(handler, http.MethodPost, "/v1/projects/adhoc/room", "", nil)
	if room.Code != http.StatusOK {
		t.Fatalf("lazy adhoc room status=%d body=%s", room.Code, room.Body.String())
	}
	rooms, _, _ = provider.snapshot()
	project, err := store.Project(context.Background(), "adhoc")
	if err != nil || project == nil || project.RoomID != "room-adhoc" || len(rooms) != 2 {
		t.Fatalf("persisted lazy room=%+v ensure calls=%v err=%v", project, rooms, err)
	}

	origin := apiActor()
	rootResponse := taskRequest(handler, http.MethodPost, "/v1/projects/adhoc/messages", `{"body":"root","subject":"Root","hostname":"host","event_at":7}`, origin)
	if rootResponse.Code != http.StatusOK {
		t.Fatalf("external root status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
	}
	var root agentProjectMessageReceipt
	if err := json.Unmarshal(rootResponse.Body.Bytes(), &root); err != nil || root.ExternalMessageID != "provider-1" || root.ParentMessageID != nil {
		t.Fatalf("external root receipt=%+v err=%v", root, err)
	}
	parentPath := fmt.Sprintf("/v1/projects/adhoc/messages/%d/reply", root.MessageID)
	replyResponse := taskRequest(handler, http.MethodPost, parentPath, `{"body":"reply"}`, origin)
	if replyResponse.Code != http.StatusOK {
		t.Fatalf("external reply status=%d body=%s", replyResponse.Code, replyResponse.Body.String())
	}
	var reply agentProjectMessageReceipt
	if err := json.Unmarshal(replyResponse.Body.Bytes(), &reply); err != nil || reply.ExternalMessageID != "provider-2" || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID {
		t.Fatalf("external reply receipt=%+v err=%v", reply, err)
	}
	deepPath := fmt.Sprintf("/v1/projects/adhoc/messages/%d/action", reply.MessageID)
	deepResponse := taskRequest(handler, http.MethodPost, deepPath, `{"body":"deep action"}`, origin)
	if deepResponse.Code != http.StatusOK {
		t.Fatalf("external nested action status=%d body=%s", deepResponse.Code, deepResponse.Body.String())
	}
	var deep agentProjectMessageReceipt
	if err := json.Unmarshal(deepResponse.Body.Bytes(), &deep); err != nil || deep.ExternalMessageID != "provider-3" || deep.ParentMessageID == nil || *deep.ParentMessageID != reply.MessageID {
		t.Fatalf("external nested action receipt=%+v err=%v", deep, err)
	}
	_, structured, _ := provider.snapshot()
	if len(structured) != 3 || structured[0].parent != nil || structured[0].message != (channels.OutboundMessage{AgentID: origin.SessionID, Hostname: "host", Subject: "Root", Body: "root", EventAt: 7}) || structured[1].parent == nil || *structured[1].parent != "provider-1" || structured[2].parent == nil || *structured[2].parent != "provider-2" {
		t.Fatalf("structured provider calls=%+v", structured)
	}
	thread, err := store.GetProjectMessageThread(context.Background(), "adhoc", root.MessageID)
	if err != nil || len(thread.Replies) != 1 || thread.Replies[0].ExternalMessageID == nil || *thread.Replies[0].ExternalMessageID != "provider-2" || thread.Replies[0].ParentMessageID == nil || *thread.Replies[0].ParentMessageID != root.MessageID {
		t.Fatalf("stored external thread=%+v err=%v", thread, err)
	}
	storedDeep, err := store.ProjectMessage(context.Background(), "adhoc", deep.MessageID)
	if err != nil || storedDeep.ParentMessageID == nil || *storedDeep.ParentMessageID != reply.MessageID || storedDeep.ExternalMessageID == nil || *storedDeep.ExternalMessageID != "provider-3" {
		t.Fatalf("stored nested action=%+v err=%v", storedDeep, err)
	}

	provider.failStructured = true
	failure := taskRequest(handler, http.MethodPost, "/v1/projects/adhoc/messages", `{"body":"do not store"}`, origin)
	if failure.Code != http.StatusInternalServerError {
		t.Fatalf("provider failure status=%d body=%s", failure.Code, failure.Body.String())
	}
	list, err := store.ListProjectMessageThreads(context.Background(), "adhoc", storage.ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || len(list.Threads) != 1 || list.Threads[0].ReplyCount != 1 {
		t.Fatalf("provider failure changed stored messages: list=%+v err=%v", list, err)
	}
}

func TestConfiguredHumanExternalDeliveryStoresBeforeSendAndKeepsFailure(t *testing.T) {
	store, _ := taskAPI(t)
	provider := &fakeOutbound{}
	handler, err := NewHandlerWithConfig(store, "task-key", HandlerConfig{Channels: map[string]channels.Outbound{"external": provider}})
	if err != nil {
		t.Fatal(err)
	}
	register := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"human","channel":"external"}`, nil)
	if register.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", register.Code, register.Body.String())
	}
	post := taskRequest(handler, http.MethodPost, "/v1/projects/human/messages/human", `{"content":"  hello  ","author":" Alice "}`, nil)
	if post.Code != http.StatusOK {
		t.Fatalf("human post status=%d body=%s", post.Code, post.Body.String())
	}
	var receipt humanProjectMessageResponse
	if err := json.Unmarshal(post.Body.Bytes(), &receipt); err != nil || !receipt.Delivered || receipt.DeliveryError != nil {
		t.Fatalf("human delivery receipt=%+v err=%v", receipt, err)
	}
	stored, err := store.ProjectMessage(context.Background(), "human", receipt.MessageID)
	if err != nil || stored.ExternalMessageID == nil || *stored.ExternalMessageID != "provider-1" || stored.Content != "hello" {
		t.Fatalf("human stored provider receipt=%+v err=%v", stored, err)
	}
	_, _, raw := provider.snapshot()
	if len(raw) != 1 || raw[0].text != "**Alice** (via gateway): hello" || raw[0].parent != nil {
		t.Fatalf("raw human provider calls=%+v", raw)
	}

	provider.mu.Lock()
	provider.failText = true
	provider.mu.Unlock()
	failed := taskRequest(handler, http.MethodPost, "/v1/projects/human/messages/human", `{"content":"retain me"}`, nil)
	if failed.Code != http.StatusOK || !strings.Contains(failed.Body.String(), "delivery_error") || !strings.Contains(failed.Body.String(), "text send failed") {
		t.Fatalf("failed human delivery status=%d body=%s", failed.Code, failed.Body.String())
	}
	var failedReceipt humanProjectMessageResponse
	if err := json.Unmarshal(failed.Body.Bytes(), &failedReceipt); err != nil || failedReceipt.Delivered || failedReceipt.DeliveryError == nil {
		t.Fatalf("failed human delivery receipt=%+v err=%v", failedReceipt, err)
	}
	failedRow, err := store.ProjectMessage(context.Background(), "human", failedReceipt.MessageID)
	if err != nil || failedRow.Content != "retain me" || failedRow.ExternalMessageID != nil {
		t.Fatalf("failed human send did not retain only durable row: %+v err=%v", failedRow, err)
	}
}

func TestBrowserSessionHumanMessageUsesDisplayNameAndResolvesAsUsername(t *testing.T) {
	store, _, user := browserSessionTestFixture(t)
	ctx := context.Background()
	const project = "browser-human"
	if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: "external", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	parent, err := store.CreateHumanProjectMessage(ctx, project, storage.HumanProjectMessageInput{Content: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeOutbound{}
	handler, err := NewHandlerWithConfig(store, browserSessionTestAPIKey, HandlerConfig{Channels: map[string]channels.Outbound{"external": provider}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	token := authn.MintBrowserSession(browserSessionTestAPIKey, *user, time.Now().Add(time.Hour).UnixMilli())
	path := "/v1/projects/" + project + "/messages/human"
	payload := fmt.Sprintf(`{"content":"reply","parent_message_id":%d,"resolve":true}`, parent.ID)
	response, body := browserSessionRequest(t, srv, http.MethodPost, path, payload, token, "", true)
	var receipt humanProjectMessageResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &receipt) != nil || !receipt.Delivered {
		t.Fatalf("browser reply status=%d body=%s", response.StatusCode, body)
	}
	assertMessageResolution(t, store, project, parent.ID, true, "user:"+user.Username)
	response, body = browserSessionRequest(t, srv, http.MethodPost, path, `{"content":"message","author":" Explicit label "}`, token, "", true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("browser post status=%d body=%s", response.StatusCode, body)
	}
	_, _, calls := provider.snapshot()
	if len(calls) != 2 || calls[0].text != "**"+user.DisplayName+"** (via gateway): reply" || calls[1].text != "**Explicit label** (via gateway): message" {
		t.Fatalf("browser provider calls=%+v", calls)
	}
}

func TestUnconfiguredExternalAdhocProjectStaysLazyAndUnavailable(t *testing.T) {
	store, handler := taskAPI(t)
	registered := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"lazy","channel":"missing-provider"}`, nil)
	if registered.Code != http.StatusOK {
		t.Fatalf("unconfigured adhoc registration status=%d body=%s", registered.Code, registered.Body.String())
	}
	var project projectRegistration
	if err := json.Unmarshal(registered.Body.Bytes(), &project); err != nil || project.Room != "" || project.Kind != "adhoc" {
		t.Fatalf("unconfigured adhoc registration=%+v err=%v", project, err)
	}
	room := taskRequest(handler, http.MethodPost, "/v1/projects/lazy/room", "", nil)
	if room.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured lazy room status=%d body=%s", room.Code, room.Body.String())
	}
	send := taskRequest(handler, http.MethodPost, "/v1/projects/lazy/messages", `{"body":"no fallback"}`, apiActor())
	if send.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured external send status=%d body=%s", send.Code, send.Body.String())
	}
	list, err := store.ListProjectMessageThreads(context.Background(), "lazy", storage.ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || len(list.Threads) != 0 {
		t.Fatalf("unconfigured external path stored a message: %+v err=%v", list, err)
	}
}
