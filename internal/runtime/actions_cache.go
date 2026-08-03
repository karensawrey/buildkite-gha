package runtime

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	actionsCacheResultsURL       = "https://isaacsu-ghacs.buildkite.dev/"
	actionsCacheMintOrigin       = "https://agent.buildkite.com"
	actionsCacheMintTimeout      = 30 * time.Second
	actionsCachePhaseTimeout     = 14 * time.Minute
	actionsCacheMintAttempts     = 3
	maxActionsCacheTokenBytes    = 64 * 1024
	maxActionsCacheResponseBytes = 128 * 1024
)

// ActionsCacheTokenSource mints a fresh, job-scoped runtime token immediately
// before one verified actions/cache lifecycle phase executes.
type ActionsCacheTokenSource interface {
	Mint(context.Context) (string, error)
}

// AgentActionsCacheTokenSource mints tokens from the fixed Buildkite Agent API
// origin. Its dependencies are private so production callers cannot redirect
// credentials; package tests inject only the HTTP transport and clock hooks.
type AgentActionsCacheTokenSource struct {
	client *http.Client
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

// NewAgentActionsCacheTokenSource constructs the production cache token
// source. Ambient proxy configuration and redirects are deliberately disabled.
func NewAgentActionsCacheTokenSource() *AgentActionsCacheTokenSource {
	return newAgentActionsCacheTokenSource(nil)
}

func newAgentActionsCacheTokenSource(roundTripper http.RoundTripper) *AgentActionsCacheTokenSource {
	if roundTripper == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		roundTripper = transport
	}
	return &AgentActionsCacheTokenSource{
		client: &http.Client{
			Transport: roundTripper,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:  time.Now,
		wait: waitForActionsCacheRetry,
	}
}

// Mint reads the Buildkite job credential lazily and uses at most three total
// requests under one shared deadline.
func (s *AgentActionsCacheTokenSource) Mint(ctx context.Context) (string, error) {
	jobID := os.Getenv("BUILDKITE_JOB_ID")
	if !validUUID(jobID) {
		return "", fmt.Errorf("mint actions/cache runtime token: BUILDKITE_JOB_ID must be a UUID")
	}
	agentToken := os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN")
	if agentToken == "" {
		return "", fmt.Errorf("mint actions/cache runtime token: BUILDKITE_AGENT_ACCESS_TOKEN is unavailable")
	}
	if s == nil || s.client == nil {
		return "", fmt.Errorf("mint actions/cache runtime token: HTTP client is unavailable")
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, actionsCacheMintTimeout)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= actionsCacheMintAttempts; attempt++ {
		token, retryAfter, retry, err := s.mintOnce(deadlineCtx, jobID, agentToken)
		if err == nil {
			return token, nil
		}
		lastErr = err
		if !retry || attempt == actionsCacheMintAttempts || deadlineCtx.Err() != nil {
			break
		}
		delay := actionsCacheBackoff(attempt)
		if retryAfterDelay := parseActionsCacheRetryAfter(retryAfter, s.clock()()); retryAfterDelay > delay {
			delay = retryAfterDelay
		}
		if deadline, ok := deadlineCtx.Deadline(); ok {
			remaining := time.Until(deadline)
			if delay > remaining {
				delay = remaining
			}
		}
		if delay <= 0 {
			break
		}
		if err := s.waiter()(deadlineCtx, delay); err != nil {
			lastErr = err
			break
		}
	}
	if deadlineCtx.Err() != nil {
		return "", fmt.Errorf("mint actions/cache runtime token: %w", deadlineCtx.Err())
	}
	return "", fmt.Errorf("mint actions/cache runtime token: %w", lastErr)
}

func (s *AgentActionsCacheTokenSource) mintOnce(ctx context.Context, jobID, agentToken string) (token, retryAfter string, retry bool, err error) {
	endpoint := actionsCacheMintOrigin + "/v3/jobs/" + jobID + "/ghac_tokens"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", false, err
	}
	request.Header.Set("Authorization", "Token "+agentToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", false, ctx.Err()
		}
		return "", "", true, fmt.Errorf("request failed")
	}
	defer func() { _ = response.Body.Close() }()
	requestID := safeActionsCacheRequestID(response.Header)
	if response.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("unexpected HTTP status %d%s", response.StatusCode, requestID)
		return "", response.Header.Get("Retry-After"), response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500, statusErr
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", "", false, fmt.Errorf("response has invalid JSON content type%s", requestID)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxActionsCacheResponseBytes+1))
	if err != nil {
		return "", "", false, fmt.Errorf("read bounded response%s: %w", requestID, err)
	}
	if len(body) > maxActionsCacheResponseBytes {
		return "", "", false, fmt.Errorf("response exceeds the %d-byte limit%s", maxActionsCacheResponseBytes, requestID)
	}
	var payload struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return "", "", false, fmt.Errorf("decode token response%s: %w", requestID, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON documents")
		}
		return "", "", false, fmt.Errorf("decode token response%s: %w", requestID, err)
	}
	if err := validateActionsCacheToken(payload.Token); err != nil {
		return "", "", false, fmt.Errorf("decode token response%s: %w", requestID, err)
	}
	return payload.Token, "", false, nil
}

func validateActionsCacheToken(token string) error {
	if token == "" {
		return fmt.Errorf("token is empty")
	}
	if len(token) > maxActionsCacheTokenBytes {
		return fmt.Errorf("token exceeds the %d-byte limit", maxActionsCacheTokenBytes)
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(value[i])) {
			return false
		}
	}
	return true
}

func actionsCacheBackoff(attempt int) time.Duration {
	base := 250 * time.Millisecond << (attempt - 1)
	var random [8]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return base
	}
	jitter := time.Duration(binary.LittleEndian.Uint64(random[:]) % uint64(base/2+1))
	return base/2 + jitter
}

func parseActionsCacheRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds >= 0 {
		return seconds
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func safeActionsCacheRequestID(headers http.Header) string {
	for _, name := range []string{"X-Request-ID", "X-Buildkite-Request-ID"} {
		value := headers.Get(name)
		if value == "" || len(value) > 128 {
			continue
		}
		safe := true
		for i := range value {
			if value[i] < 0x21 || value[i] > 0x7e {
				safe = false
				break
			}
		}
		if safe {
			return fmt.Sprintf(" (request ID %q)", value)
		}
	}
	return ""
}

func waitForActionsCacheRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *AgentActionsCacheTokenSource) clock() func() time.Time {
	if s.now != nil {
		return s.now
	}
	return time.Now
}

func (s *AgentActionsCacheTokenSource) waiter() func(context.Context, time.Duration) error {
	if s.wait != nil {
		return s.wait
	}
	return waitForActionsCacheRetry
}
