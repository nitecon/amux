package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

type pluginEnablementStore interface {
	PluginEnabled(context.Context, string) (bool, error)
}

func enabledStep(store pluginEnablementStore, pluginID, nilStoreError string) router.Step {
	return func(ctx context.Context, _ router.Event, _ *router.StepContext) router.Outcome {
		if store == nil {
			return router.Outcome{Kind: router.Fail, Error: nilStoreError}
		}
		enabled, err := store.PluginEnabled(ctx, pluginID)
		if err != nil {
			return router.Outcome{Kind: router.Fail, Error: err.Error()}
		}
		if !enabled {
			return router.Outcome{Kind: router.Stop}
		}
		return router.Outcome{Kind: router.Continue}
	}
}
