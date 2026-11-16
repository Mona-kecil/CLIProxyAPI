package auth

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const claudeUsageBody = `{
	"five_hour": {"utilization": 0.0, "resets_at": null},
	"seven_day": {"utilization": 67.0, "resets_at": "2026-11-17T23:00:00.065908+00:00"},
	"seven_day_opus": null,
	"extra_usage": {"is_enabled": false}
}`

func claudeUsageBodyResetting(resetAt time.Time, percent int) []byte {
	return []byte(`{"five_hour":null,"seven_day":{"utilization":` + strconv.Itoa(percent) +
		`.0,"resets_at":"` + resetAt.UTC().Format("2006-01-02T15:04:05.000000-07:00") + `"}}`)
}

func TestParseClaudeUsage(t *testing.T) {
	reset := strconv.FormatInt(time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC).Unix(), 10)
	for _, tc := range []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "real shape with unstarted five hour window",
			body: claudeUsageBody,
			want: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":       reset,
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.67",
				"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
			},
		},
		{
			name: "both windows, full weekly quota is rejected",
			body: `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-14T10:00:00Z"},"seven_day":{"utilization":100.0,"resets_at":"2026-11-17T23:00:00Z"}}`,
			want: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC).Unix(), 10),
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.125",
				"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-7d-Reset":       reset,
				"Anthropic-Ratelimit-Unified-7d-Utilization": "1",
				"Anthropic-Ratelimit-Unified-7d-Status":      "rejected",
			},
		},
		{
			name: "non utc offset",
			body: `{"seven_day":{"utilization":1,"resets_at":"2026-11-18T00:00:00+01:00"}}`,
			want: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":       reset,
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.01",
				"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
			},
		},
		{name: "null windows", body: `{"five_hour":null,"seven_day":null}`, want: map[string]string{}},
		{name: "no windows", body: `{}`, want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClaudeUsage([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseClaudeUsage() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("signals = %v, want %v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s = %q, want %q (all: %v)", key, got[key], want, got)
				}
			}
		})
	}

	for name, body := range map[string]string{
		"not json":      `<html>rate limited</html>`,
		"wrong shape":   `[]`,
		"bad timestamp": `{"seven_day":{"utilization":5,"resets_at":"tomorrow"}}`,
		"empty":         ``,
	} {
		t.Run("malformed "+name, func(t *testing.T) {
			if got, err := parseClaudeUsage([]byte(body)); err == nil {
				t.Fatalf("parseClaudeUsage() = %v, want error", got)
			}
		})
	}
}

func TestParsedUsageIsConsumedBySelectorRanking(t *testing.T) {
	signals, err := parseClaudeUsage(claudeUsageBodyResetting(time.Now().Add(time.Hour), 100))
	if err != nil {
		t.Fatal(err)
	}
	rank := rankByQuotaReset(quotaResetTestAuth("a", "claude", signals), time.Now())
	if !rank.known || !rank.exhausted {
		t.Fatalf("rank = %+v, want known and exhausted", rank)
	}
}

func newBackfillManager(t *testing.T, fetch func(context.Context, *Auth) ([]byte, error), auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.claudeUsageFetch = fetch
	for _, auth := range auths {
		if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
			t.Fatal(err)
		}
	}
	return manager
}

func managerAuths(t *testing.T, manager *Manager, ids ...string) []*Auth {
	t.Helper()
	var out []*Auth
	for _, id := range ids {
		auth, ok := manager.GetByID(id)
		if !ok {
			t.Fatalf("auth %s missing", id)
		}
		out = append(out, auth)
	}
	return out
}

func TestBackfillClaudeQuotaMakesSelectorPreferSoonerReset(t *testing.T) {
	now := time.Now()
	observed := quotaResetTestAuth("observed", "claude", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10),
	})
	unknown := quotaResetTestAuth("unknown", "claude", nil)
	unknown.Quota.BackoffLevel = 2
	unknown.FileName = "/auths/claude-unknown.json"
	manager := newBackfillManager(t, func(_ context.Context, auth *Auth) ([]byte, error) {
		if auth.ID != "unknown" {
			t.Errorf("fetched %s, want only unknown", auth.ID)
		}
		return claudeUsageBodyResetting(now.Add(24*time.Hour), 67), nil
	}, observed, unknown)

	selector := &SoonestResetSelector{}
	before, err := selector.Pick(t.Context(), "claude", "", cliproxyexecutor.Options{}, managerAuths(t, manager, "unknown", "observed"))
	if err != nil || before.ID != "observed" {
		t.Fatalf("before backfill Pick() = %v, %v; want observed", before, err)
	}

	manager.backfillClaudeQuota(t.Context(), nil, 0)

	after, err := selector.Pick(t.Context(), "claude", "", cliproxyexecutor.Options{}, managerAuths(t, manager, "observed", "unknown"))
	if err != nil || after.ID != "unknown" {
		t.Fatalf("after backfill Pick() = %v, %v; want unknown", after, err)
	}
	filled, _ := manager.GetByID("unknown")
	if filled.Quota.BackoffLevel != 2 || filled.Quota.Exceeded || !filled.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("cooldown fields changed: %+v", filled.Quota)
	}
	if filled.Quota.ObservedAt.IsZero() {
		t.Fatal("ObservedAt not set")
	}
}

func TestBackfillClaudeQuotaSkipsAccountsThatNeedNoLookup(t *testing.T) {
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	known := quotaResetTestAuth("known", "claude", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": future})
	disabled := quotaResetTestAuth("disabled", "claude", nil)
	disabled.Disabled = true
	other := quotaResetTestAuth("other", "codex", nil)
	var calls atomic.Int32
	manager := newBackfillManager(t, func(context.Context, *Auth) ([]byte, error) {
		calls.Add(1)
		return claudeUsageBodyResetting(time.Now().Add(time.Hour), 10), nil
	}, known, disabled, other)

	manager.backfillClaudeQuota(t.Context(), nil, 0)

	if got := calls.Load(); got != 0 {
		t.Fatalf("fetch called %d times, want 0", got)
	}
}

func TestBackfillClaudeQuotaRefetchesStaleReset(t *testing.T) {
	past := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	stale := quotaResetTestAuth("stale", "claude", map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": past})
	next := time.Now().Add(48 * time.Hour)
	manager := newBackfillManager(t, func(context.Context, *Auth) ([]byte, error) {
		return claudeUsageBodyResetting(next, 5), nil
	}, stale)

	manager.backfillClaudeQuota(t.Context(), nil, 0)

	got, _ := manager.GetByID("stale")
	if rank := rankByQuotaReset(got, time.Now()); !rank.known || rank.resetAt.Unix() != next.Unix() {
		t.Fatalf("rank = %+v, want known reset %v", rank, next)
	}
}

func TestBackfillClaudeQuotaFailuresLeaveAuthUnknownAndRetry(t *testing.T) {
	unknown := quotaResetTestAuth("unknown", "claude", nil)
	healthy := quotaResetTestAuth("healthy", "claude", nil)
	failing := true
	manager := newBackfillManager(t, func(_ context.Context, auth *Auth) ([]byte, error) {
		switch {
		case auth.ID == "unknown" && failing:
			return nil, errors.New("usage endpoint returned status 429")
		case auth.ID == "healthy":
			return []byte(`<html>not json</html>`), nil
		}
		return claudeUsageBodyResetting(time.Now().Add(time.Hour), 1), nil
	}, unknown, healthy)

	manager.backfillClaudeQuota(t.Context(), nil, 0)
	for _, id := range []string{"unknown", "healthy"} {
		got, _ := manager.GetByID(id)
		if rankByQuotaReset(got, time.Now()).known || len(got.Quota.Signals) != 0 {
			t.Fatalf("%s filled after failed lookup: %v", id, got.Quota.Signals)
		}
	}

	failing = false
	manager.backfillClaudeQuota(t.Context(), nil, 0)
	got, _ := manager.GetByID("unknown")
	if !rankByQuotaReset(got, time.Now()).known {
		t.Fatal("next pass did not retry the failed account")
	}
}

func TestBackfillClaudeQuotaDoesNotOverwriteSignalsLearnedDuringFetch(t *testing.T) {
	auth := quotaResetTestAuth("racing", "claude", nil)
	realReset := time.Now().Add(96 * time.Hour)
	var manager *Manager
	manager = newBackfillManager(t, func(ctx context.Context, _ *Auth) ([]byte, error) {
		_, _, err := manager.Modify(ctx, "racing", 0, func(current *Auth) error {
			current.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(realReset.Unix(), 10)}
			current.Quota.ObservedAt = time.Now()
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		return claudeUsageBodyResetting(time.Now().Add(time.Hour), 10), nil
	}, auth)

	manager.backfillClaudeQuota(t.Context(), nil, 0)

	got, _ := manager.GetByID("racing")
	if rank := rankByQuotaReset(got, time.Now()); rank.resetAt.Unix() != realReset.Unix() {
		t.Fatalf("reset = %v, want the response-observed %v", rank.resetAt, realReset)
	}
}

func TestClaudeUsageBackfillLoopRepeatsUntilCancelled(t *testing.T) {
	unknown := quotaResetTestAuth("unknown", "claude", nil)
	var calls atomic.Int32
	manager := newBackfillManager(t, func(context.Context, *Auth) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("offline")
	}, unknown)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		manager.runClaudeUsageBackfill(ctx, nil, 5*time.Millisecond, 0)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d fetches before deadline", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}

func TestBackfillClaudeQuotaHoldsBackFailedLookupsUntilRetryTime(t *testing.T) {
	failing := quotaResetTestAuth("failing", "claude", nil)
	expired := quotaResetTestAuth("expired", "claude", nil)
	calls := map[string]int{}
	manager := newBackfillManager(t, func(_ context.Context, auth *Auth) ([]byte, error) {
		calls[auth.ID]++
		if auth.ID == "expired" {
			return nil, errClaudeTokenNotReady
		}
		return nil, errors.New("usage endpoint returned status 429")
	}, failing, expired)
	retryAt := map[string]time.Time{}

	manager.backfillClaudeQuota(t.Context(), retryAt, time.Hour)
	manager.backfillClaudeQuota(t.Context(), retryAt, time.Hour)

	if calls["failing"] != 1 {
		t.Fatalf("failed lookup retried %d times within the retry delay, want 1 attempt", calls["failing"])
	}
	if calls["expired"] != 2 {
		t.Fatalf("expired-token account checked %d times, want every pass (2)", calls["expired"])
	}
}

func TestFetchClaudeUsageSkipsExpiredTokenWithoutRequest(t *testing.T) {
	auth := quotaResetTestAuth("expired", "claude", nil)
	auth.Metadata = map[string]any{"access_token": "tok", "expired": time.Now().Add(-time.Minute).Format(time.RFC3339)}
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	if _, err := manager.fetchClaudeUsage(t.Context(), auth); !errors.Is(err, errClaudeTokenNotReady) {
		t.Fatalf("fetchClaudeUsage() error = %v, want errClaudeTokenNotReady", err)
	}
}

func TestStartClaudeUsageBackfillRanksAccountsBeforeReturning(t *testing.T) {
	first := quotaResetTestAuth("first", "claude", nil)
	second := quotaResetTestAuth("second", "claude", nil)
	reset := time.Now().Add(24 * time.Hour)
	manager := newBackfillManager(t, func(context.Context, *Auth) ([]byte, error) {
		return claudeUsageBodyResetting(reset, 10), nil
	}, first, second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	manager.StartClaudeUsageBackfill(ctx)

	for _, id := range []string{"first", "second"} {
		got, _ := manager.GetByID(id)
		if !rankByQuotaReset(got, time.Now()).known {
			t.Fatalf("%s still unknown when StartClaudeUsageBackfill returned", id)
		}
	}
}
