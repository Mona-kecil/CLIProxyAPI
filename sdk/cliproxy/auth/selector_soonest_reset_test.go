package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaResetTestAuth(id, provider string, signals map[string]string) *Auth {
	auth := &Auth{ID: id, Provider: provider, Quota: QuotaState{Signals: make(map[string]string, len(signals))}}
	for key, value := range signals {
		auth.Quota.Signals[http.CanonicalHeaderKey(key)] = value
	}
	return auth
}

func TestRankByQuotaReset(t *testing.T) {
	now := time.Unix(1794000000, 0)
	future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	known := quotaResetRank{known: true, resetAt: now.Add(time.Hour)}
	exhausted := known
	exhausted.exhausted = true
	for _, tc := range []struct {
		name     string
		provider string
		signals  map[string]string
		want     quotaResetRank
	}{
		{name: "unused", provider: "claude"},
		{name: "unsupported provider", provider: "gemini", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future}},
		{name: "claude allowed", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future, "Anthropic-Ratelimit-Unified-7d-Status": "allowed"}, want: known},
		{name: "claude warning", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future, "Anthropic-Ratelimit-Unified-7d-Status": "allowed_warning", "Anthropic-Ratelimit-Unified-7d-Utilization": "0.86"}, want: known},
		{name: "claude weekly rejected", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future, "Anthropic-Ratelimit-Unified-7d-Status": "rejected"}, want: exhausted},
		{name: "claude short window rejected", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future, "Anthropic-Ratelimit-Unified-5h-Reset": future, "Anthropic-Ratelimit-Unified-5h-Status": "rejected"}, want: exhausted},
		{name: "claude short window stale", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future, "Anthropic-Ratelimit-Unified-5h-Reset": past, "Anthropic-Ratelimit-Unified-5h-Status": "rejected"}, want: known},
		{name: "claude short window alone exhausted", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": future, "Anthropic-Ratelimit-Unified-5h-Status": "rejected"}, want: quotaResetRank{exhausted: true}},
		{name: "claude weekly stale", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": past, "Anthropic-Ratelimit-Unified-7d-Status": "rejected"}},
		{name: "reset at now is stale", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(now.Unix(), 10), "Anthropic-Ratelimit-Unified-7d-Status": "rejected"}},
		{name: "claude garbage", provider: "claude", signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "garbage", "Anthropic-Ratelimit-Unified-7d-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": "999999999999999999999999"}},
		{name: "canonical headers and whitespace", provider: " CLAUDE ", signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": " " + future + " ", "ANTHROPIC-RATELIMIT-UNIFIED-7D-STATUS": " REJECTED "}, want: exhausted},
		{name: "codex primary weekly", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": future, "X-Codex-Primary-Used-Percent": "97", "X-Codex-Secondary-Window-Minutes": "0"}, want: known},
		{name: "codex secondary weekly", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": past, "X-Codex-Primary-Used-Percent": "100", "X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Reset-At": future, "X-Codex-Secondary-Used-Percent": "97"}, want: known},
		{name: "codex secondary exhausted", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "300", "X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Reset-At": future, "X-Codex-Secondary-Used-Percent": "100.5"}, want: exhausted},
		{name: "codex primary exhausted", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": future, "X-Codex-Primary-Used-Percent": "100"}, want: exhausted},
		{name: "codex zero window ignored", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "0", "X-Codex-Primary-Reset-At": future, "X-Codex-Primary-Used-Percent": "100"}},
		{name: "codex stale including limit flag", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": past, "X-Codex-Primary-Used-Percent": "100", "X-Codex-Limit-Reached": "true"}},
		{name: "codex garbage", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "nope", "X-Codex-Secondary-Window-Minutes": "-1", "X-Codex-Primary-Reset-At": "garbage", "X-Codex-Primary-Used-Percent": "NaN", "X-Codex-Limit-Reached": "garbage"}},
		{name: "codex invalid reset", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": "bad", "X-Codex-Primary-Used-Percent": "100"}},
		{name: "codex invalid percent", provider: "codex", signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": future, "X-Codex-Primary-Used-Percent": "+Inf"}, want: known},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rankByQuotaReset(quotaResetTestAuth("test", tc.provider, tc.signals), now)
			if got != tc.want {
				t.Fatalf("rank = %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, flag := range []string{"true", "1", "TRUE", "yes", "on", "t"} {
		t.Run("codex limit flag "+flag, func(t *testing.T) {
			auth := quotaResetTestAuth("test", "codex", map[string]string{"X-Codex-Limit-Reached": flag})
			if got := rankByQuotaReset(auth, now); got != (quotaResetRank{exhausted: true}) {
				t.Fatalf("rank = %+v, want exhausted with unknown reset", got)
			}
		})
	}
	if got := rankByQuotaReset(nil, now); got != (quotaResetRank{}) {
		t.Fatalf("nil auth rank = %+v, want unknown", got)
	}
}

func TestSoonestResetSelectorPick(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		resetA    time.Duration
		resetB    time.Duration
		statusA   string
		statusB   string
		priorityA string
		priorityB string
		disabledA bool
		want      string
	}{
		{name: "soonest weekly reset", resetA: 48 * time.Hour, resetB: time.Hour, want: "b"},
		{name: "exhausted loses", resetA: time.Hour, resetB: 48 * time.Hour, statusA: "rejected", want: "b"},
		{name: "stale rejected is unknown", resetA: -time.Hour, resetB: time.Hour, statusA: "rejected", want: "b"},
		{name: "unknown after known", resetB: time.Hour, want: "b"},
		{name: "healthy unknown before exhausted", resetB: time.Hour, statusB: "rejected", want: "a"},
		{name: "priority before reset", resetA: 48 * time.Hour, resetB: time.Hour, priorityA: "5", priorityB: "1", want: "a"},
		{name: "equal reset keeps ID order", resetA: time.Hour, resetB: time.Hour, want: "a"},
		{name: "unknown keeps ID order", want: "a"},
		{name: "disabled account skipped", resetA: time.Hour, resetB: 48 * time.Hour, disabledA: true, want: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := quotaResetTestAuth("a", "claude", nil)
			b := quotaResetTestAuth("b", "claude", nil)
			for auth, duration := range map[*Auth]time.Duration{a: tc.resetA, b: tc.resetB} {
				if duration != 0 {
					auth.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Reset")] = strconv.FormatInt(now.Add(duration).Unix(), 10)
				}
			}
			a.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Status")] = tc.statusA
			b.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Status")] = tc.statusB
			a.Attributes = map[string]string{"priority": tc.priorityA}
			b.Attributes = map[string]string{"priority": tc.priorityB}
			a.Disabled = tc.disabledA
			got, err := (&SoonestResetSelector{}).Pick(t.Context(), "claude", "", cliproxyexecutor.Options{}, []*Auth{b, a})
			if err != nil || got == nil || got.ID != tc.want {
				t.Fatalf("Pick() = %v, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestSoonestResetSelectorCodexWebsocketPreference(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		t.Run(strconv.FormatBool(websocket), func(t *testing.T) {
			a := quotaResetTestAuth("a", "codex", map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)})
			b := quotaResetTestAuth("b", "codex", nil)
			b.Attributes = map[string]string{"websockets": "true"}
			ctx := context.Background()
			want := "a"
			if websocket {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
				want = "b"
			}
			got, err := (&SoonestResetSelector{}).Pick(ctx, "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b})
			if err != nil || got == nil || got.ID != want {
				t.Fatalf("Pick() = %v, %v; want %s", got, err, want)
			}
		})
	}
}

func TestSoonestResetSessionAffinityPreservesBinding(t *testing.T) {
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	t.Cleanup(selector.Stop)
	a := quotaResetTestAuth("a", "claude", nil)
	b := quotaResetTestAuth("b", "claude", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)})
	for _, tc := range []struct {
		session string
		want    string
	}{
		{session: "existing", want: "b"},
		{session: "existing", want: "b"},
		{session: "new", want: "a"},
	} {
		opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: tc.session}}
		got, err := selector.Pick(t.Context(), "claude", "model", opts, []*Auth{a, b})
		if err != nil || got == nil || got.ID != tc.want {
			t.Fatalf("session %s: Pick() = %v, %v; want %s", tc.session, got, err, tc.want)
		}
		a.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Reset")] = strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)
		b.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Status")] = "rejected"
	}
}

func TestSoonestResetManagerPreservesAliasAvailability(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(strconv.FormatBool(mixed), func(t *testing.T) {
			withQuotaCooldownEnabled(t)
			manager := NewManager(nil, &SoonestResetSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			const route, target = "soonest-alias", "soonest-target"
			for _, id := range []string{"a-" + t.Name(), "b-" + t.Name()} {
				auth := quotaResetTestAuth(id, "claude", nil)
				if id[0] == 'b' {
					auth.Quota.Signals[http.CanonicalHeaderKey("Anthropic-Ratelimit-Unified-7d-Reset")] = strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: route}, {ID: target}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
					t.Fatal(err)
				}
				retryAfter := time.Hour
				manager.MarkResult(t.Context(), Result{AuthID: id, Provider: "claude", Model: route, RetryAfter: &retryAfter, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"}})
			}
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: target, Alias: route, Fork: true}}})
			var got *Auth
			var err error
			if mixed {
				got, _, _, err = manager.pickNextMixed(t.Context(), []string{"claude"}, route, cliproxyexecutor.Options{}, nil)
			} else {
				got, _, err = manager.pickNext(t.Context(), "claude", route, cliproxyexecutor.Options{}, nil)
			}
			if err != nil || got == nil || got.ID != "b-"+t.Name() {
				t.Fatalf("manager pick = %v, %v; want b-%s", got, err, t.Name())
			}
		})
	}
}
