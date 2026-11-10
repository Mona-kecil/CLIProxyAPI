package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type quotaResetRank struct {
	exhausted bool
	known     bool
	resetAt   time.Time
}

func rankByQuotaReset(auth *Auth, now time.Time) quotaResetRank {
	var rank quotaResetRank
	if auth == nil {
		return rank
	}
	signal := func(key string) string {
		return strings.TrimSpace(auth.Quota.Signals[http.CanonicalHeaderKey(key)])
	}
	reset := func(key string) time.Time {
		seconds, err := strconv.ParseInt(signal(key), 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}
		}
		return time.Unix(seconds, 0)
	}

	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		weeklyReset := reset("Anthropic-Ratelimit-Unified-7d-Reset")
		if weeklyReset.After(now) {
			rank.known, rank.resetAt = true, weeklyReset
			rank.exhausted = strings.EqualFold(signal("Anthropic-Ratelimit-Unified-7d-Status"), "rejected")
		}
		if reset("Anthropic-Ratelimit-Unified-5h-Reset").After(now) &&
			strings.EqualFold(signal("Anthropic-Ratelimit-Unified-5h-Status"), "rejected") {
			rank.exhausted = true
		}
	case "codex":
		var weeklyPrefix string
		var weeklyMinutes int64
		for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
			minutes, err := strconv.ParseInt(signal(prefix+"Window-Minutes"), 10, 64)
			if err == nil && minutes > weeklyMinutes {
				weeklyPrefix, weeklyMinutes = prefix, minutes
			}
		}
		var weeklyReset time.Time
		if weeklyPrefix != "" {
			weeklyReset = reset(weeklyPrefix + "Reset-At")
			if weeklyReset.After(now) {
				rank.known, rank.resetAt = true, weeklyReset
				used, err := strconv.ParseFloat(signal(weeklyPrefix+"Used-Percent"), 64)
				rank.exhausted = err == nil && !math.IsInf(used, 0) && used >= 100
			}
		}
		// A limit flag from a completed window must not keep an account exhausted.
		if weeklyReset.IsZero() || rank.known {
			switch strings.ToLower(signal("X-Codex-Limit-Reached")) {
			case "true", "1", "t", "yes", "on":
				rank.exhausted = true
			}
		}
	}
	return rank
}

type SoonestResetSelector struct{}

func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ranks := make(map[*Auth]quotaResetRank, len(available))
	for _, auth := range available {
		ranks[auth] = rankByQuotaReset(auth, now)
	}
	sort.SliceStable(available, func(i, j int) bool {
		left, right := ranks[available[i]], ranks[available[j]]
		if left.exhausted != right.exhausted {
			return !left.exhausted
		}
		if left.known != right.known {
			return left.known
		}
		return left.resetAt.Before(right.resetAt)
	})
	return available[0], nil
}
