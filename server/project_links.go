package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
)

type projectLinkRequest struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type projectLinkDeleteResponse struct {
	Changed bool `json:"changed"`
}

func (a *api) registerProjectLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{ident}/links", a.listProjectLinks)
	mux.HandleFunc("POST /v1/projects/{ident}/links", a.addProjectLink)
	mux.HandleFunc("DELETE /v1/projects/{ident}/links/{id}", a.deleteProjectLink)
}

func (a *api) listProjectLinks(w http.ResponseWriter, r *http.Request) {
	links, err := a.store.ProjectLinks(r.Context(), r.PathValue("ident"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read project links")
		return
	}
	writeJSON(w, links)
}

func (a *api) addProjectLink(w http.ResponseWriter, r *http.Request) {
	if !projectLinkJSONContentType(r.Header.Get("Content-Type")) {
		writeProjectLinkExtractorError(w, http.StatusUnsupportedMediaType, "Expected request with `Content-Type: application/json`")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProjectLinkExtractorError(w, http.StatusRequestEntityTooLarge, "Failed to buffer the request body")
		} else {
			writeProjectLinkExtractorError(w, http.StatusBadRequest, "Failed to buffer the request body")
		}
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			writeProjectLinkExtractorError(w, http.StatusBadRequest, "Failed to parse the request body as JSON")
		} else {
			writeProjectLinkExtractorError(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type")
		}
		return
	}
	if fields == nil {
		writeProjectLinkExtractorError(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type")
		return
	}
	var request projectLinkRequest
	for _, field := range []struct {
		name   string
		target *string
	}{{"label", &request.Label}, {"url", &request.URL}} {
		key, target := field.name, field.target
		raw, ok := fields[key]
		if !ok {
			writeProjectLinkExtractorError(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: missing field `"+key+"`")
			return
		}
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, target) != nil {
			writeProjectLinkExtractorError(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: `"+key+"` must be a string")
			return
		}
	}
	link, err := a.store.AddProjectLink(r.Context(), r.PathValue("ident"), request.Label, request.URL)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrProjectLinkProjectNotFound):
			writeError(w, http.StatusBadRequest, "project '"+r.PathValue("ident")+"' not found")
		case errors.Is(err, storage.ErrInvalidProjectLink):
			writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), storage.ErrInvalidProjectLink.Error()+": "))
		default:
			writeError(w, http.StatusInternalServerError, "cannot add project link")
		}
		return
	}
	writeJSON(w, link)
}

func (a *api) deleteProjectLink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project link ID")
		return
	}
	changed, err := a.store.DeleteProjectLink(r.Context(), r.PathValue("ident"), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete project link")
		return
	}
	writeJSON(w, projectLinkDeleteResponse{Changed: changed})
}

func projectLinkJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	typeParts := strings.SplitN(mediaType, "/", 2)
	return len(typeParts) == 2 && strings.EqualFold(typeParts[0], "application") && (strings.EqualFold(typeParts[1], "json") || strings.HasSuffix(strings.ToLower(typeParts[1]), "+json"))
}

func writeProjectLinkExtractorError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
}
