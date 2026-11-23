package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nitecon/amux/storage"
)

type patternCreateRequest struct {
	Title        string          `json:"title"`
	Slug         *string         `json:"slug"`
	Summary      *string         `json:"summary"`
	Body         string          `json:"body"`
	Labels       json.RawMessage `json:"labels"`
	Version      string          `json:"version"`
	State        string          `json:"state"`
	SupersededBy *string         `json:"superseded_by"`
	Author       *string         `json:"author"`
}

type patternUpdateRequest struct {
	Title        *string         `json:"title"`
	Slug         *string         `json:"slug"`
	Summary      json.RawMessage `json:"summary"`
	Body         *string         `json:"body"`
	Labels       json.RawMessage `json:"labels"`
	Version      *string         `json:"version"`
	State        *string         `json:"state"`
	SupersededBy json.RawMessage `json:"superseded_by"`
}

type patternCommentRequest struct {
	Content    string  `json:"content"`
	Author     *string `json:"author"`
	AuthorType *string `json:"author_type"`
}

func (a *api) registerPatternRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/patterns", a.listPatterns)
	mux.HandleFunc("POST /v1/patterns", a.createPattern)
	mux.HandleFunc("GET /v1/patterns/{id}", a.getPattern)
	mux.HandleFunc("PATCH /v1/patterns/{id}", a.updatePattern)
	mux.HandleFunc("DELETE /v1/patterns/{id}", a.deletePattern)
	mux.HandleFunc("GET /v1/patterns/{id}/comments", a.listPatternComments)
	mux.HandleFunc("POST /v1/patterns/{id}/comments", a.addPatternComment)
}

func (a *api) listPatterns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filters := storage.PatternFilters{}
	if value, ok := query["q"]; ok {
		filters.Query = firstQueryValue(value)
	}
	if value, ok := query["label"]; ok {
		filters.Label = firstQueryValue(value)
	}
	if value, ok := query["version"]; ok {
		filters.Version = firstQueryValue(value)
		if err := validatePatternVersionField(strings.TrimSpace(*filters.Version)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if value, ok := query["state"]; ok {
		filters.State = firstQueryValue(value)
		if err := validatePatternStateField(strings.TrimSpace(*filters.State)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if value, ok := query["superseded_by"]; ok {
		filters.SupersededBy = firstQueryValue(value)
	}
	items, err := a.store.ListPatterns(r.Context(), filters)
	if err != nil {
		patternError(w, err)
		return
	}
	writeJSON(w, items)
}

func (a *api) createPattern(w http.ResponseWriter, r *http.Request) {
	var request patternCreateRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	if strings.TrimSpace(request.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(request.Body) == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	version, state := strings.TrimSpace(request.Version), strings.TrimSpace(request.State)
	if err := validatePatternVersionField(version); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePatternStateField(state); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	supersededBy := cleanPatternString(request.SupersededBy)
	if version == "superseded" && supersededBy == nil {
		writeError(w, http.StatusBadRequest, "superseded_by is required when version is superseded")
		return
	}
	if version != "superseded" && supersededBy != nil {
		writeError(w, http.StatusBadRequest, "superseded_by can only be set when version is superseded")
		return
	}
	labels, err := decodePatternLabels("labels", request.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	slug := cleanPatternString(request.Slug)
	summary := cleanPatternString(request.Summary)
	item, err := a.store.InsertPattern(r.Context(), storage.PatternInsert{
		Title: strings.TrimSpace(request.Title), Slug: slug, Summary: summary, Body: request.Body,
		Labels: labels, Version: version, State: state, SupersededBy: supersededBy,
		Author: resolveTaskIdentity(request.Author, r),
	})
	if err != nil {
		patternError(w, err)
		return
	}
	writeJSON(w, item)
}

func (a *api) getPattern(w http.ResponseWriter, r *http.Request) {
	item, err := a.store.GetPattern(r.Context(), r.PathValue("id"))
	if err != nil {
		patternError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "pattern '"+r.PathValue("id")+"' not found")
		return
	}
	writeJSON(w, item)
}

func (a *api) updatePattern(w http.ResponseWriter, r *http.Request) {
	var request patternUpdateRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	labels, err := decodeOptionalPatternLabels("labels", request.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := decodeNullablePatternString("summary", request.Summary)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	supersededBy, err := decodeNullablePatternString("superseded_by", request.SupersededBy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Version != nil {
		value := strings.TrimSpace(*request.Version)
		if err := validatePatternVersionField(value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Version = &value
	}
	if request.State != nil {
		value := strings.TrimSpace(*request.State)
		if err := validatePatternStateField(value); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request.State = &value
	}
	update := storage.PatternUpdate{Summary: summary, Labels: labels, SupersededBy: supersededBy, Version: request.Version, State: request.State}
	if request.Title != nil {
		if value := strings.TrimSpace(*request.Title); value != "" {
			update.Title = &value
		}
	}
	if request.Slug != nil {
		if value := strings.TrimSpace(*request.Slug); value != "" {
			update.Slug = &value
		}
	}
	if request.Body != nil {
		update.Body = request.Body
	}
	item, err := a.store.UpdatePattern(r.Context(), r.PathValue("id"), update)
	if err != nil {
		patternError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "pattern '"+r.PathValue("id")+"' not found")
		return
	}
	writeJSON(w, item)
}

func (a *api) deletePattern(w http.ResponseWriter, r *http.Request) {
	deleted, err := a.store.DeletePattern(r.Context(), r.PathValue("id"))
	if err != nil {
		patternError(w, err)
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "pattern '"+r.PathValue("id")+"' not found")
		return
	}
	writeJSON(w, map[string]bool{"deleted": true})
}

func (a *api) listPatternComments(w http.ResponseWriter, r *http.Request) {
	items, found, err := a.store.ListPatternComments(r.Context(), r.PathValue("id"))
	if err != nil {
		patternError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "pattern '"+r.PathValue("id")+"' not found")
		return
	}
	writeJSON(w, items)
}

func (a *api) addPatternComment(w http.ResponseWriter, r *http.Request) {
	var request patternCommentRequest
	if !readJSONRequest(w, r, &request, 1<<20) {
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	authorType := "user"
	if strings.TrimSpace(r.Header.Get("X-Agent-Id")) != "" && strings.TrimSpace(r.Header.Get("X-Agent-Id")) != "_default" {
		authorType = "agent"
	}
	if request.AuthorType != nil {
		authorType = *request.AuthorType
	}
	if authorType != "agent" && authorType != "user" && authorType != "system" {
		writeError(w, http.StatusBadRequest, "invalid author_type '"+authorType+"': must be agent|user|system")
		return
	}
	comment, err := a.store.InsertPatternComment(r.Context(), r.PathValue("id"), resolveTaskIdentity(request.Author, r), authorType, request.Content)
	if err != nil {
		patternError(w, err)
		return
	}
	if comment == nil {
		writeError(w, http.StatusNotFound, "pattern '"+r.PathValue("id")+"' not found")
		return
	}
	writeJSON(w, comment)
}

func firstQueryValue(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	return &values[0]
}

func cleanPatternString(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func decodePatternLabels(field string, raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []string{}, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		labels := make([]string, 0, len(list))
		for _, item := range list {
			var label string
			if err := json.Unmarshal(item, &label); err != nil {
				return nil, fmt.Errorf("'%s' must be an array of strings or a comma-separated string", field)
			}
			if label = strings.TrimSpace(label); label != "" {
				labels = append(labels, label)
			}
		}
		return labels, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		labels := []string{}
		for _, value := range strings.Split(text, ",") {
			if value = strings.TrimSpace(value); value != "" {
				labels = append(labels, value)
			}
		}
		return labels, nil
	}
	return nil, fmt.Errorf("'%s' must be an array of strings or a comma-separated string", field)
}

func decodeOptionalPatternLabels(field string, raw json.RawMessage) (*[]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	labels, err := decodePatternLabels(field, raw)
	if err != nil {
		return nil, err
	}
	return &labels, nil
}

func decodeNullablePatternString(field string, raw json.RawMessage) (**string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value *string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &value, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("'%s' must be a string or null", field)
	}
	value = &text
	return &value, nil
}

func validatePatternVersionField(value string) error {
	if !storage.IsPatternVersion(value) {
		return fmt.Errorf("invalid version '%s': must be draft|latest|superseded", value)
	}
	return nil
}

func validatePatternStateField(value string) error {
	if !storage.IsPatternState(value) {
		return fmt.Errorf("invalid state '%s': must be active|archived", value)
	}
	return nil
}

func patternError(w http.ResponseWriter, err error) {
	message := err.Error()
	if strings.Contains(message, "invalid pattern") || strings.Contains(message, "superseded_by") || strings.Contains(message, "invalid author_type") {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	writeError(w, http.StatusInternalServerError, "pattern operation failed")
}
