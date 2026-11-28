package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

type pluginEnablementStore interface {
	PluginEnabled(context.Context, string) (bool, error)
}

func enabledStep(store pluginEnablementStore, pluginID, nilStoreError string) fism.Step {
	return func(ctx context.Context, _ fism.Event, _ *fism.StepContext) fism.Outcome {
		if store == nil {
			return fism.Outcome{Kind: fism.Fail, Error: nilStoreError}
		}
		enabled, err := store.PluginEnabled(ctx, pluginID)
		if err != nil {
			return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
		}
		if !enabled {
			return fism.Outcome{Kind: fism.Stop}
		}
		return fism.Outcome{Kind: fism.Continue}
	}
}
