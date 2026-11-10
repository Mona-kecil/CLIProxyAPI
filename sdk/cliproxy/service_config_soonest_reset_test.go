package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestNormalizedRoutingRuntimeStateSoonestReset(t *testing.T) {
	for _, input := range []string{"soonest-reset", "soonest-reset-first", "srf", " SRF "} {
		t.Run(input, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(&internalconfig.Config{
				Routing: internalconfig.RoutingConfig{Strategy: input},
			})
			if state.strategy != "soonest-reset" {
				t.Fatalf("strategy = %q, want soonest-reset", state.strategy)
			}
			got, err := newRoutingSelector(state).Pick(t.Context(), "claude", "", cliproxyexecutor.Options{}, []*coreauth.Auth{
				{ID: "a", Provider: "claude"},
				{ID: "b", Provider: "claude", Quota: coreauth.QuotaState{Signals: map[string]string{
					"Anthropic-Ratelimit-Unified-7d-Reset": "4102444800",
				}}},
			})
			if err != nil || got == nil || got.ID != "b" {
				t.Fatalf("Pick() = %v, %v; want b", got, err)
			}
		})
	}
}
