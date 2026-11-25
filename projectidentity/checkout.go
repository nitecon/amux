package projectidentity

import (
	"context"
	"fmt"

	"github.com/nitecon/amux/projects"
)

const (
	OpenInFolder    = "Open in Folder"
	CloneInProjects = "Clone in Projects Directory"
	CloneAtLocation = "Clone at Location"
)

func ValidateBinding(kind string, remote *string, checkout Identity) error {
	if remote == nil {
		if kind == "adhoc" && checkout.Upstream == "" {
			return nil
		}
		return fmt.Errorf("selected project has no repository binding; attach an unbound checkout only to an ad-hoc project")
	}
	if checkout.Upstream == "" {
		return fmt.Errorf("checkout has no Git origin for this repository-backed project")
	}
	canonical, err := projects.CanonicalRemote(*remote)
	if err != nil {
		return fmt.Errorf("selected project has an invalid repository binding")
	}
	if canonical != checkout.Upstream {
		return fmt.Errorf("checkout origin does not match the selected project")
	}
	return nil
}

func PrepareCheckout(ctx context.Context, kind string, remote *string, method, parent, location, source string) (Identity, *string, error) {
	if method != OpenInFolder && remote != nil {
		candidate, err := projects.CanonicalRemote(source)
		canonical, canonicalErr := projects.CanonicalRemote(*remote)
		if err != nil || canonicalErr != nil || candidate != canonical {
			return Identity{}, nil, fmt.Errorf("repository URL does not match the selected project")
		}
	}
	var err error
	switch method {
	case CloneInProjects:
		location, err = CloneRepository(ctx, parent, source)
	case CloneAtLocation:
		location, err = CloneRepositoryAt(ctx, location, source)
	case OpenInFolder:
	default:
		return Identity{}, nil, fmt.Errorf("choose a checkout option")
	}
	if err != nil {
		return Identity{}, nil, err
	}
	identity, err := ResolveCheckout(ctx, location)
	if err != nil {
		return identity, nil, err
	}
	if err := ValidateBinding(kind, remote, identity); err != nil {
		return identity, nil, err
	}
	origin, err := CloneOrigin(ctx, identity.Directory)
	return identity, origin, err
}
