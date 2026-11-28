package localconversation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type createChat struct {
	Provider    string
	Preferences providers.Preferences
	Replace     string
}

type chatCommand struct {
	ID      string
	Command conversation.Command
}

type Approval struct {
	Task         bool
	Conversation storage.Conversation
	Event        storage.ConversationEvent
}

func (c Remote) WatchApprovals(ctx context.Context, consume func([]Approval) error) error {
	response, err := clientcontrol.Open(ctx, c.ConfigPath, http.MethodGet, "/approvals", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var approvals []Approval
		if err := decoder.Decode(&approvals); err != nil {
			return err
		}
		if err := consume(approvals); err != nil {
			return err
		}
	}
}

func (c Remote) ResolveTaskApproval(ctx context.Context, id, option string) error {
	payload, err := json.Marshal(conversation.Command{OptionID: option})
	if err != nil {
		return err
	}
	_, err = clientcontrol.Request(ctx, c.ConfigPath, http.MethodPost, "/approvals/"+url.PathEscape(id), payload)
	return err
}

type Remote struct {
	ConfigPath string
}

func (c Remote) Create(ctx context.Context, project, provider, replace string, prefs providers.Preferences) (storage.Conversation, error) {
	var info storage.Conversation
	payload, err := json.Marshal(createChat{Provider: provider, Preferences: prefs, Replace: replace})
	if err == nil {
		var data []byte
		data, err = clientcontrol.Request(ctx, c.ConfigPath, http.MethodPost, chatRoute(project, ""), payload)
		if err == nil {
			err = json.Unmarshal(data, &info)
		}
	}
	return info, err
}

func (c Remote) Conversation(ctx context.Context, project, id string) (*storage.Conversation, error) {
	data, err := clientcontrol.Request(ctx, c.ConfigPath, http.MethodGet, chatRoute(project, id), nil)
	if err != nil {
		return nil, err
	}
	var info storage.Conversation
	return &info, json.Unmarshal(data, &info)
}

func (Remote) Task(context.Context, string, string) (*storage.Task, error) { return nil, nil }

func (c Remote) Enqueue(ctx context.Context, project, id, commandID string, command conversation.Command) error {
	payload, err := json.Marshal(chatCommand{ID: commandID, Command: command})
	if err != nil {
		return err
	}
	_, err = clientcontrol.Request(ctx, c.ConfigPath, http.MethodPost, chatRoute(project, id)+"/commands", payload)
	return err
}

func (c Remote) Stream(ctx context.Context, project, id string, cursor gatewayclient.Cursor, consume func(gatewayclient.Frame) error) error {
	response, err := clientcontrol.Open(ctx, c.ConfigPath, http.MethodGet, chatRoute(project, id)+"/events?after="+strconv.FormatInt(cursor.Events, 10), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	for {
		var frame gatewayclient.Frame
		if err := decoder.Decode(&frame); err != nil {
			return err
		}
		if err := consume(frame); err != nil {
			return err
		}
	}
}

func chatRoute(project, id string) string {
	base := "/chats/" + url.PathEscape(project)
	if id == "" {
		return base
	}
	return base + "/" + url.PathEscape(id)
}

// RegisterControl mounts the local, authenticated conversation API. The
// provider lifetime follows the client context, so viewers can reconnect.
func RegisterControl(mux *http.ServeMux, ctx context.Context, root string, checkout func(string) string, open worker.Open, taskApprovals *TaskApprovals) func() {
	var mu sync.Mutex
	var notifyMu sync.Mutex
	changes := make(chan struct{})
	notify := func() {
		notifyMu.Lock()
		defer notifyMu.Unlock()
		close(changes)
		changes = make(chan struct{})
	}
	sessions := make(map[[2]string]*Session)
	session := func(project, id string) (*Session, error) {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := [2]string{project, id}
		if existing := sessions[key]; existing != nil {
			return existing, nil
		}
		if checkout(project) == "" {
			return nil, fmt.Errorf("project has no configured checkout")
		}
		s, err := Open(root, storage.Conversation{ProjectIdent: project, ID: id}, open)
		if err != nil {
			return nil, err
		}
		s.KeepAlive(ctx)
		s.approvalChanged = notify
		sessions[key] = s
		return s, nil
	}
	mux.HandleFunc("GET /approvals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		controller := http.NewResponseController(w)
		for {
			notifyMu.Lock()
			changed := changes
			notifyMu.Unlock()
			approvals, taskChanged := taskApprovals.snapshot()
			if approvals == nil {
				approvals = make([]Approval, 0)
			}
			mu.Lock()
			for _, s := range sessions {
				approvals = append(approvals, s.pendingApprovals()...)
			}
			mu.Unlock()
			if err := encoder.Encode(approvals); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-changed:
			case <-taskChanged:
			}
		}
	})
	if taskApprovals != nil {
		mux.HandleFunc("POST /approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
			command, err := readControl[conversation.Command](w, r)
			if err != nil {
				http.Error(w, "invalid approval response", http.StatusBadRequest)
				return
			}
			if err := taskApprovals.resolve(r.Context(), r.PathValue("id"), command.OptionID); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
			}
		})
	}
	mux.HandleFunc("POST /chats/{project}", func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		directory := checkout(project)
		input, err := readControl[createChat](w, r)
		if err != nil || directory == "" {
			http.Error(w, "invalid chat or project has no configured checkout", http.StatusBadRequest)
			return
		}
		if _, err := LaunchCommand(input.Provider, input.Preferences); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, err := Create(root, project, directory, input.Provider, input.Preferences)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if input.Replace != "" {
			mu.Lock()
			old := sessions[[2]string{project, input.Replace}]
			mu.Unlock()
			if old != nil {
				old.Stop()
			}
		}
		_ = json.NewEncoder(w).Encode(info)
	})
	withSession := func(handle func(http.ResponseWriter, *http.Request, *Session)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s, err := session(r.PathValue("project"), r.PathValue("id"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			handle(w, r, s)
		}
	}
	mux.HandleFunc("GET /chats/{project}/{id}", withSession(func(w http.ResponseWriter, r *http.Request, s *Session) {
		info, _ := s.Conversation(r.Context(), "", "")
		_ = json.NewEncoder(w).Encode(info)
	}))
	mux.HandleFunc("GET /chats/{project}/{id}/events", withSession(func(w http.ResponseWriter, r *http.Request, s *Session) {
		after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || after < 0 || after > int64(s.EventCount()) {
			http.Error(w, "invalid chat cursor", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		controller := http.NewResponseController(w)
		_ = s.Stream(r.Context(), "", "", gatewayclient.Cursor{Events: after}, func(frame gatewayclient.Frame) error {
			if err := encoder.Encode(frame); err != nil {
				return err
			}
			return controller.Flush()
		})
	}))
	mux.HandleFunc("POST /chats/{project}/{id}/commands", withSession(func(w http.ResponseWriter, r *http.Request, s *Session) {
		if checkout(r.PathValue("project")) == "" {
			http.Error(w, "project execution requires an enabled local checkout", http.StatusConflict)
			return
		}
		input, err := readControl[chatCommand](w, r)
		if err != nil || input.ID == "" {
			http.Error(w, "invalid chat command", http.StatusBadRequest)
			return
		}
		if err := s.Enqueue(r.Context(), "", "", input.ID, input.Command); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	}))
	return func() {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range sessions {
			s.Stop()
		}
	}
}

func readControl[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var result T
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&result)
	return result, err
}

func (s *Session) pendingApprovals() []Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == "" {
		return nil
	}
	var pending []Approval
	resolved := make(map[string]bool)
	for i := len(s.events) - 1; i >= 0; i-- {
		event := s.events[i]
		switch event.Event.Kind {
		case "turn_completed", "local_prompt_started":
			return pending
		case "permission_resolved":
			resolved[event.Event.RequestID] = true
		case "permission":
			if event.Event.RequestID != "" && !resolved[event.Event.RequestID] {
				pending = append(pending, Approval{Conversation: s.file.Info, Event: event})
				resolved[event.Event.RequestID] = true
			}
		}
	}
	return pending
}
