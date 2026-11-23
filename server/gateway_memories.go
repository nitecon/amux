package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

const gatewayMemoryOKFCapability = "okf-markdown-v1"

func (a *api) registerGatewayMemoryReadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/memories", a.listGatewayMemories)
	mux.HandleFunc("POST /v1/projects/{ident}/memories/pull", a.pullGatewayMemories)
	mux.HandleFunc("POST /v1/projects/{ident}/memories", a.pushGatewayMemories)
	mux.HandleFunc("POST /v1/projects/{ident}/memories/push", a.pushGatewayMemories)
	mux.HandleFunc("GET /v1/memories/projects", a.discoverGatewayMemoryProjects)
	mux.HandleFunc("GET /v1/memories/capabilities", gatewayMemoryCapabilities)
}

type gatewayMemoryPushWireRequest struct {
	Memories []storage.GatewayMemoryPushItem `json:"memories"`
}

func gatewayMemoryCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"version": 1,
		"capabilities": []map[string]any{{
			"id": gatewayMemoryOKFCapability, "version": 1, "format": "okf-markdown",
			"request_header":     "X-Memory-Capabilities",
			"max_envelope_bytes": storage.GatewayMemoryOKFMaxEnvelopeBytes,
			"max_document_bytes": storage.GatewayMemoryOKFMaxDocumentBytes,
			"max_depth":          storage.GatewayMemoryOKFMaxDepth,
			"max_nodes":          storage.GatewayMemoryOKFMaxNodes,
		}},
	})
}

func (a *api) pushGatewayMemories(w http.ResponseWriter, r *http.Request) {
	request := gatewayMemoryPushWireRequest{}
	if !decodeGatewayMemoryPushRequest(w, r, &request) {
		return
	}
	if len(request.Memories) > storage.GatewayMemoryBatchLimit {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("memory push batch exceeds %d records", storage.GatewayMemoryBatchLimit))
		return
	}
	ident := projects.NormalizeIdent(r.PathValue("ident"))
	source := storage.GatewayMemoryPushSource{
		AgentID: optionalMemoryHeader(r, "X-Agent-ID"), Hostname: optionalMemoryHeader(r, "X-Agent-Hostname"),
		ClientID: optionalMemoryHeader(r, "X-Memory-Client-ID"), ClientVersion: optionalMemoryHeader(r, "X-Memory-Client-Version"),
	}
	if source.ClientID == nil {
		source.ClientID = source.AgentID
	}
	response, err := a.store.PushGatewayMemories(r.Context(), ident, source, request.Memories)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "memory push failed")
		return
	}
	writeJSON(w, response)
}

func optionalMemoryHeader(r *http.Request, name string) *string {
	value := strings.TrimSpace(r.Header.Get(name))
	if value == "" {
		return nil
	}
	return &value
}

func decodeGatewayMemoryPushRequest(w http.ResponseWriter, r *http.Request, request *gatewayMemoryPushWireRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	if request.Memories == nil {
		request.Memories = []storage.GatewayMemoryPushItem{}
	}
	return true
}

func (a *api) listGatewayMemories(w http.ResponseWriter, r *http.Request) {
	filters, err := gatewayMemoryFilters(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	projectIdent, ok := a.gatewayMemoryProject(w, r)
	if !ok {
		return
	}
	response, err := a.store.SearchGatewayMemories(r.Context(), projectIdent, filters)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "memory search failed")
		return
	}
	if !gatewayMemoryOKFRequested(r) {
		for i := range response.Memories {
			response.Memories[i].ConceptHash = nil
			response.Memories[i].OKF = nil
		}
	}
	writeJSON(w, response)
}

func (a *api) pullGatewayMemories(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeGatewayMemoryPullRequest(w, r)
	if !ok {
		return
	}
	projectIdent, ok := a.gatewayMemoryProject(w, r)
	if !ok {
		return
	}
	response, err := a.store.PullGatewayMemories(r.Context(), projectIdent, request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "memory pull failed")
		return
	}
	if !gatewayMemoryOKFRequested(r) {
		for i := range response.Memories {
			response.Memories[i].ConceptHash = nil
			response.Memories[i].OKF = nil
		}
	}
	writeJSON(w, response)
}

func (a *api) gatewayMemoryProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	projectIdent := projects.NormalizeIdent(r.PathValue("ident"))
	if projectIdent == storage.GatewayGlobalMemoryProjectIdent {
		return projectIdent, true
	}
	project, err := a.store.Project(r.Context(), projectIdent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "project lookup failed")
		return "", false
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return "", false
	}
	return project.Ident, true
}

func (a *api) discoverGatewayMemoryProjects(w http.ResponseWriter, r *http.Request) {
	response, err := a.store.DiscoverGatewayMemoryProjects(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "memory project discovery failed")
		return
	}
	writeJSON(w, response)
}

func gatewayMemoryFilters(query url.Values) (storage.GatewayMemoryFilters, error) {
	for _, key := range []string{"q", "type", "memory_type", "tag", "since_revision", "limit", "include_tombstones"} {
		if len(query[key]) > 1 {
			return storage.GatewayMemoryFilters{}, fmt.Errorf("duplicate memory query parameter")
		}
	}
	if len(query["type"]) != 0 && len(query["memory_type"]) != 0 {
		return storage.GatewayMemoryFilters{}, fmt.Errorf("duplicate memory query parameter")
	}
	filters := storage.GatewayMemoryFilters{Query: query.Get("q"), Tag: query.Get("tag")}
	filters.MemoryType = query.Get("type")
	if filters.MemoryType == "" {
		filters.MemoryType = query.Get("memory_type")
	}
	if value, ok := query["since_revision"]; ok {
		revision, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return storage.GatewayMemoryFilters{}, fmt.Errorf("invalid since_revision")
		}
		filters.SinceRevision = &revision
	}
	if value, ok := query["limit"]; ok {
		limit, err := strconv.ParseUint(value[0], 10, 64)
		if err != nil {
			return storage.GatewayMemoryFilters{}, fmt.Errorf("invalid limit")
		}
		filters.Limit = &limit
	}
	if value, ok := query["include_tombstones"]; ok {
		switch value[0] {
		case "true":
			filters.IncludeTombstones = true
		case "false":
		default:
			return storage.GatewayMemoryFilters{}, fmt.Errorf("invalid include_tombstones")
		}
	}
	return filters, nil
}

func gatewayMemoryOKFRequested(r *http.Request) bool {
	for _, capability := range strings.Split(r.Header.Get("X-Memory-Capabilities"), ",") {
		if strings.TrimSpace(capability) == gatewayMemoryOKFCapability {
			return true
		}
	}
	return false
}

type gatewayMemoryPullWireRequest struct {
	SinceRevision     *int64                               `json:"since_revision"`
	Known             []storage.GatewayMemoryKnownRevision `json:"known"`
	PageSize          *uint64                              `json:"page_size"`
	IncludeTombstones *bool                                `json:"include_tombstones"`
}

func decodeGatewayMemoryPullRequest(w http.ResponseWriter, r *http.Request) (storage.GatewayMemoryPullRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return storage.GatewayMemoryPullRequest{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeError(w, http.StatusBadRequest, "invalid request")
		return storage.GatewayMemoryPullRequest{}, false
	}
	var wire gatewayMemoryPullWireRequest
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return storage.GatewayMemoryPullRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return storage.GatewayMemoryPullRequest{}, false
	}
	includeTombstones := true
	if wire.IncludeTombstones != nil {
		includeTombstones = *wire.IncludeTombstones
	}
	return storage.GatewayMemoryPullRequest{
		SinceRevision: wire.SinceRevision, Known: wire.Known, PageSize: wire.PageSize,
		IncludeTombstones: includeTombstones,
	}, true
}
