package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL               = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageBackfillTick      = time.Minute
	claudeUsageFailureRetryDelay = 15 * time.Minute
	claudeUsageStartupBudget     = 10 * time.Second
	claudeUsageFetchTimeout      = 15 * time.Second
	claudeUsageMaxBodyBytes      = 1 << 20
)

// errClaudeQuotaKnown aborts a Modify edit when a real response filled the
// quota signals after the usage lookup started.
var errClaudeQuotaKnown = errors.New("claude quota already known")

// errClaudeTokenNotReady marks a lookup skipped before any request because the
// access token is missing or expired. Auto-refresh renews it shortly, so the
// next tick retries instead of waiting out the failure delay.
var errClaudeTokenNotReady = errors.New("access token missing or expired")

type claudeUsageWindow struct {
	// Utilization is a percent (0-100), unlike the fractional response header.
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// parseClaudeUsage converts a usage endpoint body into the header-shaped
// signals rankByQuotaReset reads. A window with a null object or null
// resets_at (not started yet) contributes nothing.
func parseClaudeUsage(body []byte) (map[string]string, error) {
	var usage struct {
		FiveHour *claudeUsageWindow `json:"five_hour"`
		SevenDay *claudeUsageWindow `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("decode claude usage: %w", err)
	}
	signals := make(map[string]string, 6)
	for _, window := range []struct {
		prefix string
		usage  *claudeUsageWindow
	}{
		{"Anthropic-Ratelimit-Unified-5h-", usage.FiveHour},
		{"Anthropic-Ratelimit-Unified-7d-", usage.SevenDay},
	} {
		if window.usage == nil || window.usage.ResetsAt == nil {
			continue
		}
		resetsAt, err := time.Parse(time.RFC3339, *window.usage.ResetsAt)
		if err != nil {
			return nil, fmt.Errorf("parse claude usage resets_at: %w", err)
		}
		signals[window.prefix+"Reset"] = strconv.FormatInt(resetsAt.Unix(), 10)
		if window.usage.Utilization == nil {
			continue
		}
		percent := *window.usage.Utilization
		signals[window.prefix+"Utilization"] = strconv.FormatFloat(percent/100, 'f', -1, 64)
		status := "allowed"
		if percent >= 100 {
			status = "rejected"
		}
		signals[window.prefix+"Status"] = status
	}
	return signals, nil
}

// fetchClaudeUsage reads the usage endpoint, which costs no quota. It uses the
// stored access token as is and leaves renewal to the auto-refresh loop.
func (m *Manager) fetchClaudeUsage(ctx context.Context, auth *Auth) ([]byte, error) {
	if !auth.HasValidAccessToken(time.Now()) {
		return nil, errClaudeTokenNotReady
	}
	token := authAccessToken(auth)
	client := &http.Client{Timeout: claudeUsageFetchTimeout}
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	if proxyURL == "" {
		if cfg := m.runtimeConfigSnapshot(); cfg != nil {
			proxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	if proxyURL != "" {
		// A configured but unusable proxy fails the fetch instead of going direct.
		transport, _, err := proxyutil.BuildHTTPTransport(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("build proxy transport: %w", err)
		}
		if transport != nil {
			defer transport.CloseIdleConnections()
			client.Transport = transport
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, claudeUsageMaxBodyBytes))
}

// backfillClaudeQuota fills the quota signals of every enabled Claude auth
// whose weekly reset is unknown, so soonest-reset routing is correct before
// any real response has taught it. retryAt, when non-nil, holds back auths
// whose last lookup failed until their retry time.
func (m *Manager) backfillClaudeQuota(ctx context.Context, retryAt map[string]time.Time, retryDelay time.Duration) {
	fetch := m.claudeUsageFetch
	if fetch == nil {
		fetch = m.fetchClaudeUsage
	}
	for _, candidate := range m.List() {
		if ctx.Err() != nil {
			return
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "claude") ||
			candidate.Disabled || candidate.Status == StatusDisabled ||
			rankByQuotaReset(candidate, time.Now()).known ||
			time.Now().Before(retryAt[candidate.ID]) {
			continue
		}
		name := claudeAuthLogName(candidate)
		fetchCtx, cancel := context.WithTimeout(ctx, claudeUsageFetchTimeout)
		body, err := fetch(fetchCtx, candidate)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errClaudeTokenNotReady) {
			log.Debugf("claude usage backfill: %s: %v", name, err)
			continue
		}
		var signals map[string]string
		if err == nil {
			signals, err = parseClaudeUsage(body)
		}
		if err != nil {
			log.Warnf("claude usage backfill: lookup for %s failed: %v", name, err)
			if retryAt != nil {
				retryAt[candidate.ID] = time.Now().Add(retryDelay)
			}
			continue
		}
		if len(signals) == 0 {
			log.Debugf("claude usage backfill: %s reported no active windows", name)
			continue
		}
		// Quota signals are runtime-only, so skip rewriting the credential file.
		_, applied, err := m.Modify(WithSkipPersist(ctx), candidate.ID, 0, func(current *Auth) error {
			if current.Disabled || rankByQuotaReset(current, time.Now()).known {
				return errClaudeQuotaKnown
			}
			current.Quota.Signals = signals
			current.Quota.ObservedAt = time.Now()
			return nil
		})
		switch {
		case errors.Is(err, errClaudeQuotaKnown):
		case err != nil:
			log.Warnf("claude usage backfill: apply for %s failed: %v", name, err)
		case applied:
			log.Infof("claude usage backfill: filled quota reset for %s", name)
		}
	}
}

func claudeAuthLogName(auth *Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	return auth.ID
}

// StartClaudeUsageBackfill runs the first backfill pass before returning, then
// one every minute in the background until ctx is cancelled. The first pass
// blocks so the server starts serving only once every reachable account is
// ranked; otherwise clients reconnecting after a restart bind to whichever
// account's lookup finished first. Only unknown accounts are looked up, so a
// steady state makes about one request per account per weekly reset.
func (m *Manager) StartClaudeUsageBackfill(ctx context.Context) {
	retryAt := make(map[string]time.Time)
	startupCtx, cancel := context.WithTimeout(ctx, claudeUsageStartupBudget)
	m.backfillClaudeQuota(startupCtx, retryAt, claudeUsageFailureRetryDelay)
	cancel()
	go m.runClaudeUsageBackfill(ctx, retryAt, claudeUsageBackfillTick, claudeUsageFailureRetryDelay)
}

func (m *Manager) runClaudeUsageBackfill(ctx context.Context, retryAt map[string]time.Time, tick, retryDelay time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.backfillClaudeQuota(ctx, retryAt, retryDelay)
		}
	}
}
