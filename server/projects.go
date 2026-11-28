package server

import (
	"errors"
	"net/http"
	"time"

	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

type projectRegistration struct {
	Ident        string  `json:"ident"`
	Channel      string  `json:"channel_name"`
	Room         string  `json:"room_id"`
	Kind         string  `json:"kind"`
	RepoFullName *string `json:"repo_full_name"`
}

func (a *api) registerProject(w http.ResponseWriter, r *http.Request) {
	var req *struct {
		Ident    string  `json:"ident"`
		Channel  *string `json:"channel"`
		RepoURL  *string `json:"repo_url"`
		Recreate bool    `json:"recreate,omitempty"`
	}
	if !readTaskRequest(w, r, &req) {
		return
	}
	if req == nil {
		writeError(w, 400, "project must be an object")
		return
	}
	ident := projectid.SanitizeIdent(req.Ident)
	channel := "internal"
	if req.Channel != nil {
		channel = *req.Channel
	}
	var repo *projectid.RepoRef
	if req.RepoURL != nil {
		parsed, err := projectid.ParseRemote(*req.RepoURL)
		if err == nil {
			repo = &parsed
		}
	}
	if repo == nil {
		parsed, err := projectid.ParseRemote(req.Ident)
		if err == nil {
			repo = &parsed
		}
	}
	existing, err := a.store.Project(r.Context(), ident)
	if err != nil {
		taskError(w, err)
		return
	}
	if existing != nil {
		if repo != nil {
			if _, err := a.store.BackfillRepo(r.Context(), ident, *repo); err != nil {
				taskError(w, err)
				return
			}
			existing, err = a.store.Project(r.Context(), ident)
			if err != nil {
				taskError(w, err)
				return
			}
		}
	} else {
		roomID := ""
		kind := "adhoc"
		if channel == "internal" {
			roomID = ident
		} else if repo != nil {
			provider, ok := a.channels[channel]
			if !ok {
				writeError(w, http.StatusBadRequest, "unknown channel plugin: '"+channel+"'")
				return
			}
			room, err := provider.EnsureRoom(r.Context(), ident)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "channel room creation failed")
				return
			}
			roomID = room
			kind = "repo"
		}
		p := storage.Project{Ident: ident, ChannelName: channel, RoomID: roomID, CreatedAt: time.Now().UnixMilli(), Kind: kind}
		if repo != nil {
			provider, namespace, name, full, canonical := repo.Provider, repo.Namespace, repo.Name, repo.FullName(), repo.Canonical
			p.RepoProvider = &provider
			p.RepoNamespace = &namespace
			p.RepoName = &name
			p.RepoFullName = &full
			p.CanonicalRemote = &canonical
			p.Kind = "repo"
		}
		if err := a.store.RegisterProject(r.Context(), p, req.Recreate); err != nil {
			if errors.Is(err, storage.ErrProjectDeleted) {
				writeError(w, http.StatusGone, err.Error())
				return
			}
			taskError(w, err)
			return
		}
		existing, err = a.store.Project(r.Context(), ident)
		if err != nil {
			taskError(w, err)
			return
		}
	}
	if existing == nil {
		writeError(w, 500, "project registration failed")
		return
	}
	writeJSON(w, projectRegistration{existing.Ident, existing.ChannelName, existing.RoomID, existing.Kind, existing.RepoFullName})
}

func (a *api) deleteProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	if actor != nil {
		writeError(w, http.StatusForbidden, "project deletion requires a human request")
		return
	}
	var req struct {
		Confirmation string `json:"confirmation"`
	}
	if !readJSONRequest(w, r, &req, 4096) {
		return
	}
	deleted, err := a.store.DeleteProject(r.Context(), r.PathValue("ident"), req.Confirmation)
	switch {
	case errors.Is(err, storage.ErrProjectDeleteInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "project deletion failed")
	case !deleted:
		writeError(w, http.StatusNotFound, "project not found")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
