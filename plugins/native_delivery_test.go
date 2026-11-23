package plugins

import (
	"context"
	"testing"

	"github.com/nitecon/amux/router"
)

type nativeDeliveryFake struct{ pending bool }

func (f *nativeDeliveryFake) ReconcileNativeDelivery(context.Context, *router.Registry, router.Event) (bool, error) {
	return f.pending, nil
}

func TestNativeDeliveryPluginSubscribesWithoutFIFOOrSwitchSetting(t *testing.T) {
	plugin := NativeDeliveryPlugin(&nativeDeliveryFake{})
	if plugin.Definition.ID != "native-delivery" || plugin.Definition.FIFO {
		t.Fatalf("unexpected native delivery scheduling policy: %+v", plugin.Definition)
	}
	if len(plugin.PreExec) != 0 || len(plugin.Definition.Subscriptions) != 1 || plugin.Definition.Subscriptions[0].Event != NotifyEventID || plugin.Definition.Subscriptions[0].MinVersion != 1 || plugin.Definition.Subscriptions[0].MaxVersion != 1 || len(plugin.Definition.Subscriptions[0].DependsOn) != 0 {
		t.Fatalf("native delivery should subscribe directly to agent.notify without a new feature toggle: %+v", plugin)
	}
}
