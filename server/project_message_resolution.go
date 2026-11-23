package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type projectMessageResolutionResponse struct {
	Changed bool `json:"changed"`
}

type projectMessagesBulkResolutionResponse struct {
	Resolved int64 `json:"resolved"`
}

type projectMessagesBulkResolutionRequest struct {
	BeforeMS    *int64   `json:"before_ms"`
	AuthorKind  *string  `json:"author_kind"`
	AuthorKinds []string `json:"author_kinds"`
}

func (a *api) registerProjectMessageResolutionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/projects/{ident}/messages/resolve", a.resolveProjectMessagesBulk)
	mux.HandleFunc("POST /v1/projects/{ident}/messages/{id}/resolve", a.resolveProjectMessage)
	mux.HandleFunc("POST /v1/projects/{ident}/messages/{id}/reopen", a.reopenProjectMessage)
}

func (a *api) resolveProjectMessage(w http.ResponseWriter, r *http.Request) {
	messageID, ok := projectMessagePathID(w, r)
	if !ok {
		return
	}
	changed, err := a.store.ResolveProjectMessage(r.Context(), r.PathValue("ident"), messageID, projectMessageResolverIdentity(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "message resolution failed")
		return
	}
	writeJSON(w, projectMessageResolutionResponse{Changed: changed})
}

func (a *api) reopenProjectMessage(w http.ResponseWriter, r *http.Request) {
	messageID, ok := projectMessagePathID(w, r)
	if !ok {
		return
	}
	changed, err := a.store.ReopenProjectMessage(r.Context(), r.PathValue("ident"), messageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "message reopen failed")
		return
	}
	writeJSON(w, projectMessageResolutionResponse{Changed: changed})
}

func (a *api) resolveProjectMessagesBulk(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeProjectMessagesBulkResolution(w, r)
	if !ok {
		return
	}
	before := time.Now().UTC().UnixMilli()
	if request.BeforeMS != nil {
		before = *request.BeforeMS
	}
	kinds := make([]string, 0, len(request.AuthorKinds)+1)
	for _, kind := range request.AuthorKinds {
		if kind = strings.ToLower(strings.TrimSpace(kind)); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	if request.AuthorKind != nil {
		if kind := strings.ToLower(strings.TrimSpace(*request.AuthorKind)); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	resolved, err := a.store.ResolveProjectMessagesBefore(r.Context(), r.PathValue("ident"), before, kinds, projectMessageResolverIdentity(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bulk message resolution failed")
		return
	}
	writeJSON(w, projectMessagesBulkResolutionResponse{Resolved: resolved})
}

func projectMessagePathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	messageID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return 0, false
	}
	return messageID, true
}

func projectMessageResolverIdentity(r *http.Request) string {
	if user := authenticatedBrowserUser(r.Context()); user != nil {
		return "user:" + user.Username
	}
	if agentID := r.Header.Get("X-Agent-Id"); agentID != "" {
		return "agent:" + agentID
	}
	return "user"
}

func decodeProjectMessagesBulkResolution(w http.ResponseWriter, r *http.Request) (projectMessagesBulkResolutionRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return projectMessagesBulkResolutionRequest{}, false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return projectMessagesBulkResolutionRequest{}, true
	}
	if body[0] != '{' {
		writeError(w, http.StatusBadRequest, "invalid request")
		return projectMessagesBulkResolutionRequest{}, false
	}
	var request projectMessagesBulkResolutionRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return projectMessagesBulkResolutionRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return projectMessagesBulkResolutionRequest{}, false
	}
	return request, true
}
