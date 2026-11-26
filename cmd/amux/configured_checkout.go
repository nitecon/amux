package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/projectidentity"
)

func configuredCheckoutHandler(client *gatewayclient.Client, reloads chan<- clientReload, stopped <-chan struct{}, projectsDirectory func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input clientcontrol.CheckoutRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.Project == "" {
			http.Error(w, "invalid checkout request", http.StatusBadRequest)
			return
		}
		inventory, err := client.Projects(r.Context())
		if err != nil {
			http.Error(w, "The local amux client could not read gateway projects: "+err.Error(), http.StatusBadGateway)
			return
		}
		for _, project := range inventory {
			if project.Ident != input.Project || project.ArchivedAt != nil {
				continue
			}
			identity, origin, err := projectidentity.PrepareCheckout(r.Context(), project.Kind, project.CanonicalRemote, input.Method, projectsDirectory(), input.Destination, input.Source)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			result, err := submitClientReload(r.Context(), reloads, stopped, &clientcontrol.CheckoutChange{Project: project.Ident, Checkout: identity.Directory})
			if err != nil {
				if errors.Is(err, errClientStopping) {
					http.Error(w, err.Error(), http.StatusServiceUnavailable)
				}
				return
			}
			if strings.HasPrefix(result, "Rejected configuration:") {
				http.Error(w, result, http.StatusUnprocessableEntity)
				return
			}
			_ = json.NewEncoder(w).Encode(clientcontrol.CheckoutResult{Identity: identity, Origin: origin})
			return
		}
		http.Error(w, "gateway project is not active", http.StatusNotFound)
	}
}
