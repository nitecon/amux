package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
)

func (a *api) listProjectMessageThreads(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("ident")
	filter, err := projectMessageThreadFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.store.ListProjectMessageThreads(r.Context(), project, filter)
	if err != nil {
		if errors.Is(err, storage.ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "project message list failed")
		return
	}
	writeJSON(w, result)
}

func projectMessageThreadFilter(r *http.Request) (storage.ProjectMessageThreadFilter, error) {
	query := r.URL.Query()
	for _, key := range []string{"state", "kind", "agent_id", "limit", "offset"} {
		if len(query[key]) > 1 {
			return storage.ProjectMessageThreadFilter{}, fmt.Errorf("duplicate message query parameter")
		}
	}
	limit, err := messageListUint(query, "limit", 50)
	if err != nil {
		return storage.ProjectMessageThreadFilter{}, fmt.Errorf("invalid message limit")
	}
	if limit > 500 {
		limit = 500
	}
	offset, err := messageListUint(query, "offset", 0)
	if err != nil {
		return storage.ProjectMessageThreadFilter{}, fmt.Errorf("invalid message offset")
	}
	var kinds []string
	for _, kind := range strings.Split(query.Get("kind"), ",") {
		if kind = strings.ToLower(strings.TrimSpace(kind)); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	var agentID *string
	if values, present := query["agent_id"]; present && len(values) != 0 {
		value := values[0]
		agentID = &value
	}
	return storage.ProjectMessageThreadFilter{State: query.Get("state"), Kinds: kinds, AgentID: agentID, Limit: limit, Offset: offset}, nil
}

func messageListUint(query map[string][]string, key string, fallback uint64) (uint64, error) {
	values, present := query[key]
	if !present {
		return fallback, nil
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("missing value")
	}
	value := values[0]
	if strings.HasPrefix(value, "+") {
		value = value[1:]
	}
	return strconv.ParseUint(value, 10, 64)
}
