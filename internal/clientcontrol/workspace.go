package clientcontrol

import (
	"context"
	"encoding/json"
	"net/http"

	projectid "github.com/nitecon/amux/identity/project"
)

type Workspace struct {
	ProjectsDirectory string
	Projects          map[string]string
	ExcludedProjects  map[string]bool
	LocalCheckouts    []projectid.LocalCheckout
}

func LoadWorkspace(ctx context.Context, configPath string) (Workspace, error) {
	var workspace Workspace
	data, err := Request(ctx, configPath, http.MethodGet, "/workspace", nil)
	if err != nil {
		return workspace, err
	}
	err = json.Unmarshal(data, &workspace)
	return workspace, err
}

type CheckoutRequest struct {
	Project     string
	Method      string
	Destination string
	Source      string
}

type CheckoutResult struct {
	Identity projectid.Identity
	Origin   *string
}

func PrepareCheckout(ctx context.Context, configPath string, input CheckoutRequest) (CheckoutResult, error) {
	var checkout CheckoutResult
	data, err := json.Marshal(input)
	if err != nil {
		return checkout, err
	}
	response, err := Open(ctx, configPath, http.MethodPost, "/checkout", data)
	if err != nil {
		return checkout, err
	}
	defer response.Body.Close()
	err = json.NewDecoder(response.Body).Decode(&checkout)
	return checkout, err
}
