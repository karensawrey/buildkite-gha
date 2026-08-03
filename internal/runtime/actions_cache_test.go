package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

const (
	testBuildkiteJobID      = "123e4567-e89b-12d3-a456-426614174000"
	testBuildkiteAgentToken = "parent-agent-secret"
)

type actionsCacheRoundTripFunc func(*http.Request) (*http.Response, error)

func (f actionsCacheRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func actionsCacheHTTPResponse(status int, contentType, body string) *http.Response {
	response := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	return response
}

func setActionsCacheMintEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("BUILDKITE_JOB_ID", testBuildkiteJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", testBuildkiteAgentToken)
}

func TestAgentActionsCacheTokenSourceExactRequest(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	var calls int
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.String() != actionsCacheMintOrigin+"/v3/jobs/"+testBuildkiteJobID+"/ghac_tokens" || string(body) != "{}" {
			t.Fatalf("request = %s %s %q", request.Method, request.URL, body)
		}
		if request.Header.Get("Authorization") != "Token "+testBuildkiteAgentToken || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers = %#v", request.Header)
		}
		return actionsCacheHTTPResponse(http.StatusOK, "application/json; charset=utf-8", `{"token":"phase-token","extra":true}`), nil
	}))
	token, err := source.Mint(context.Background())
	if err != nil || token != "phase-token" || calls != 1 {
		t.Fatalf("Mint() = %q, %v; calls = %d", token, err, calls)
	}
}

func TestAgentActionsCacheTokenSourceValidatesAmbientIdentityBeforeNetwork(t *testing.T) {
	tests := []struct {
		name, jobID, token, want string
	}{
		{name: "missing job", token: testBuildkiteAgentToken, want: "must be a UUID"},
		{name: "malformed job", jobID: "../../redirect", token: testBuildkiteAgentToken, want: "must be a UUID"},
		{name: "missing agent token", jobID: testBuildkiteJobID, want: "is unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BUILDKITE_JOB_ID", test.jobID)
			t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", test.token)
			calls := 0
			source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network")
			}))
			if _, err := source.Mint(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) || calls != 0 {
				t.Fatalf("Mint() error = %v, calls = %d", err, calls)
			}
		})
	}
}

func TestAgentActionsCacheTokenSourceRejectsUnsafeResponses(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	tests := []struct {
		name, contentType, body, want string
	}{
		{name: "missing content type", body: `{"token":"x"}`, want: "content type"},
		{name: "wrong content type", contentType: "text/plain", body: `{"token":"x"}`, want: "content type"},
		{name: "malformed", contentType: "application/json", body: `{`, want: "decode token response"},
		{name: "missing token", contentType: "application/json", body: `{}`, want: "token is empty"},
		{name: "empty token", contentType: "application/json", body: `{"token":""}`, want: "token is empty"},
		{name: "trailing document", contentType: "application/json", body: `{"token":"x"}{}`, want: "token response"},
		{name: "oversized token", contentType: "application/json", body: `{"token":"` + strings.Repeat("x", maxActionsCacheTokenBytes+1) + `"}`, want: "token exceeds"},
		{name: "oversized body", contentType: "application/json", body: strings.Repeat(" ", maxActionsCacheResponseBytes+1), want: "response exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return actionsCacheHTTPResponse(http.StatusOK, test.contentType, test.body), nil
			}))
			if token, err := source.Mint(context.Background()); err == nil || token != "" || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Mint() = %q, %v", token, err)
			}
		})
	}
}

func TestAgentActionsCacheTokenSourceRetryPolicyAndRetryAfter(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	now := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		statuses   []int
		retryAfter string
		wantCalls  int
		wantWait   time.Duration
		wantErr    bool
	}{
		{name: "429 then success", statuses: []int{429, 200}, retryAfter: "2", wantCalls: 2, wantWait: 2 * time.Second},
		{name: "HTTP date then success", statuses: []int{503, 200}, retryAfter: now.Add(3 * time.Second).Format(http.TimeFormat), wantCalls: 2, wantWait: 3 * time.Second},
		{name: "three server failures", statuses: []int{500, 502, 503}, wantCalls: 3, wantErr: true},
		{name: "ordinary client failure", statuses: []int{400, 200}, wantCalls: 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(*http.Request) (*http.Response, error) {
				status := test.statuses[calls]
				calls++
				response := actionsCacheHTTPResponse(status, "application/json", `{"token":"ok"}`)
				if status != http.StatusOK {
					response.Header.Set("Retry-After", test.retryAfter)
				}
				return response, nil
			}))
			source.now = func() time.Time { return now }
			var waits []time.Duration
			source.wait = func(_ context.Context, delay time.Duration) error {
				waits = append(waits, delay)
				return nil
			}
			token, err := source.Mint(context.Background())
			if (err != nil) != test.wantErr || calls != test.wantCalls {
				t.Fatalf("Mint() = %q, %v; calls = %d, waits = %v", token, err, calls, waits)
			}
			if test.wantWait > 0 && (len(waits) == 0 || waits[0] != test.wantWait) {
				t.Fatalf("waits = %v, want first %s", waits, test.wantWait)
			}
		})
	}
}

func TestAgentActionsCacheTokenSourceRetriesConnectionFailureAndStopsOnCancellation(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	calls := 0
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("dial failed with " + testBuildkiteAgentToken)
		}
		return actionsCacheHTTPResponse(http.StatusOK, "application/json", `{"token":"ok"}`), nil
	}))
	source.wait = func(context.Context, time.Duration) error { return nil }
	if token, err := source.Mint(context.Background()); err != nil || token != "ok" || calls != 2 {
		t.Fatalf("Mint() = %q, %v; calls = %d", token, err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if _, err := source.Mint(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled Mint() error = %v, calls = %d", err, calls)
	}
}

func TestAgentActionsCacheTokenSourceRejectsRedirectWithoutForwardingAuthorization(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	var mu sync.Mutex
	var requests []*http.Request
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		requests = append(requests, request.Clone(request.Context()))
		mu.Unlock()
		response := actionsCacheHTTPResponse(http.StatusFound, "text/plain", "do not include this body")
		response.Header.Set("Location", "https://attacker.invalid/token")
		response.Request = request
		return response, nil
	}))
	_, err := source.Mint(context.Background())
	if err == nil || strings.Contains(err.Error(), "do not include this body") || strings.Contains(err.Error(), testBuildkiteAgentToken) {
		t.Fatalf("Mint() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].URL.Host != "agent.buildkite.com" {
		t.Fatalf("redirect requests = %#v", requests)
	}
}

func TestAgentActionsCacheTokenSourceErrorsOmitCredentialsAndResponseBody(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	minted := "minted-runtime-secret"
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(*http.Request) (*http.Response, error) {
		response := actionsCacheHTTPResponse(http.StatusForbidden, "text/plain", testBuildkiteAgentToken+" "+minted+" response-secret")
		response.Header.Set("X-Request-ID", "safe-request-id")
		return response, nil
	}))
	_, err := source.Mint(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "safe-request-id") {
		t.Fatalf("Mint() error = %v", err)
	}
	for _, secret := range []string{testBuildkiteAgentToken, minted, "response-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Mint() error contains %q: %v", secret, err)
		}
	}
}

func TestAgentActionsCacheTokenSourceProductionTransportIgnoresAmbientProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://attacker.invalid:8080")
	source := NewAgentActionsCacheTokenSource()
	transport, ok := source.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("production transport = %#v, want no proxy function", source.client.Transport)
	}
}

func TestAgentActionsCacheTokenSourceSharedDeadline(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	calls := 0
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := source.Mint(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("Mint() error = %v, calls = %d", err, calls)
	}
}

func TestAgentActionsCacheTokenSourceDoesNotDependOnEndpointEnvironment(t *testing.T) {
	setActionsCacheMintEnvironment(t)
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "https://attacker.invalid")
	seen := ""
	source := newAgentActionsCacheTokenSource(actionsCacheRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = request.URL.String()
		return actionsCacheHTTPResponse(http.StatusOK, "application/json", `{"token":"ok"}`), nil
	}))
	if _, err := source.Mint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, actionsCacheMintOrigin+"/") {
		t.Fatalf("mint URL = %q", seen)
	}
}

type sequenceActionsCacheTokenSource struct {
	mu     sync.Mutex
	tokens []string
	calls  int
}

func (s *sequenceActionsCacheTokenSource) Mint(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.tokens) {
		return "", fmt.Errorf("unexpected cache token mint %d", s.calls+1)
	}
	token := s.tokens[s.calls]
	s.calls++
	return token, nil
}

type markerActionsCacheRedactor struct {
	dir    string
	values []string
	err    error
}

func (r *markerActionsCacheRedactor) AddRedaction(_ context.Context, value string) error {
	r.values = append(r.values, value)
	if r.err != nil {
		return r.err
	}
	return os.WriteFile(filepath.Join(r.dir, value), []byte("registered"), 0o600)
}

func actionsCacheRuntimeFixture(t *testing.T, operation actionintegration.ActionsCacheOperation) (string, string, plan.Job, *fakeActionMaterializer) {
	t.Helper()
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: cache runtime test\n")
	remote := t.TempDir()
	path := ""
	metadataSource := "name: cache\nruns:\n  using: node24\n  main: main.js\n"
	if operation == actionintegration.ActionsCacheRoot {
		metadataSource += "  post: post.js\n  post-if: always()\n"
	} else {
		path = string(operation)
	}
	writeFixtureFile(t, remote, filepath.Join(path, "action.yml"), metadataSource)
	writeFixtureFile(t, remote, filepath.Join(path, "main.js"), "")
	if operation == actionintegration.ActionsCacheRoot {
		writeFixtureFile(t, remote, "post.js", "")
	}
	digest := digestTree(t, remote)
	lockID := "a-0000000000000001"
	uses := "actions/cache@v4.2.0"
	if path != "" {
		uses = "actions/cache/" + path + "@v4.2.0"
	}
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "cache", Kind: "uses", Uses: uses, Action: &plan.ActionSelector{Lock: lockID}}})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{{
		ID: lockID, Source: "github", Repository: "actions/cache", RequestedRef: "v4.2.0",
		Commit: strings.Repeat("a", 40), Path: path, SourceDigest: digest,
	}}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: filepath.Join(remote, path), SourceDigest: digest}}
	return workspace, remote, job, materializer
}

func writeActionsCacheFakeNode(t *testing.T, workspace, script string) string {
	t.Helper()
	node := filepath.Join(workspace, "node24")
	writeFixtureFile(t, workspace, "node24", "#!/bin/sh\nset -eu\nif [ \"${1:-}\" = --version ]; then echo v24.0.0; exit 0; fi\n"+script)
	if err := os.Chmod(node, 0o700); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestActionsCacheRootMintsFreshProtectedTokensForMainAndPost(t *testing.T) {
	workspace, _, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRoot)
	redactorDir := t.TempDir()
	events := filepath.Join(workspace, "events")
	node := writeActionsCacheFakeNode(t, workspace, `
test "$ACTIONS_RESULTS_URL" = "https://isaacsu-ghacs.buildkite.dev/"
test "$ACTIONS_CACHE_SERVICE_V2" = true
test -n "$ACTIONS_RUNTIME_TOKEN"
test -f "$REDACTOR_DIR/$ACTIONS_RUNTIME_TOKEN"
test "$SEGMENT_DOWNLOAD_TIMEOUT_MINS" = 7
for name in ACTIONS_CACHE_URL ACTIONS_RUNTIME_URL NODE_OPTIONS NODE_PATH NODE_EXTRA_CA_CERTS NODE_TLS_REJECT_UNAUTHORIZED SSLKEYLOGFILE LD_PRELOAD LD_LIBRARY_PATH HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy BUILDKITE_AGENT_ACCESS_TOKEN BUILDKITE_JOB_ID; do
  if env | grep -q "^${name}="; then exit 91; fi
done
phase=$(basename "$1" .js)
printf '%s:%s\n' "$phase" "$ACTIONS_RUNTIME_TOKEN" >> "$CACHE_EVENTS"
if [ "$phase" = main ]; then
  printf '%s\n' 'cache-hit=true' >> "$GITHUB_OUTPUT"
  printf '%s\n' 'cache_state=retained' >> "$GITHUB_STATE"
else
  test "$STATE_cache_state" = retained
  printf '%s\n' 'post-summary' >> "$GITHUB_STEP_SUMMARY"
fi
`)
	job.Outputs = map[string]string{"cache-hit": "${{ steps.cache.outputs.cache-hit }}"}
	job.Steps = append([]plan.Step{{
		ID: "prior-environment", Kind: "run", Shell: "sh",
		Command: `printf '%s\n' 'NODE_PATH=/effect-injected' 'https_proxy=http://effect-proxy' 'ACTIONS_RUNTIME_TOKEN=effect-token' >> "$GITHUB_ENV"`,
	}}, job.Steps...)
	job.Env = map[string]string{
		"CACHE_EVENTS": events, "REDACTOR_DIR": redactorDir, "SEGMENT_DOWNLOAD_TIMEOUT_MINS": "7",
		"ACTIONS_RESULTS_URL": "https://attacker.invalid/", "ACTIONS_RUNTIME_TOKEN": "workflow-token", "ACTIONS_CACHE_SERVICE_V2": "false",
		"ACTIONS_CACHE_URL": "https://legacy.invalid/", "ACTIONS_RUNTIME_URL": "https://legacy.invalid/",
		"NODE_OPTIONS": "--require attacker", "NODE_PATH": "/attacker", "NODE_EXTRA_CA_CERTS": "/attacker.pem", "NODE_TLS_REJECT_UNAUTHORIZED": "0",
		"SSLKEYLOGFILE": "/tmp/keys", "LD_PRELOAD": "/attacker.so", "LD_LIBRARY_PATH": "/attacker",
		"HTTP_PROXY": "http://attacker", "HTTPS_PROXY": "http://attacker", "ALL_PROXY": "http://attacker", "NO_PROXY": "agent.buildkite.com",
		"http_proxy": "http://attacker", "https_proxy": "http://attacker", "all_proxy": "http://attacker", "no_proxy": "agent.buildkite.com",
		"BUILDKITE_AGENT_ACCESS_TOKEN": "parent-agent-value", "BUILDKITE_JOB_ID": testBuildkiteJobID,
	}
	tokens := &sequenceActionsCacheTokenSource{tokens: []string{"root-main-token", "root-post-token"}}
	redactor := &markerActionsCacheRedactor{dir: redactorDir}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: redactor, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	contents, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "main:root-main-token\npost:root-post-token\n"; got != want {
		t.Fatalf("cache lifecycle = %q, want %q", got, want)
	}
	if tokens.calls != 2 || !slices.Equal(redactor.values, []string{"root-main-token", "root-post-token"}) {
		t.Fatalf("token/redactor calls = %d / %#v", tokens.calls, redactor.values)
	}
	if result.Outputs["cache-hit"] != "true" || result.State["cache_state"] != "retained" || result.Summary != "post-summary\n" {
		t.Fatalf("upstream effects = %#v", result)
	}
	for _, token := range tokens.tokens {
		if strings.Contains(logs.String(), token) || strings.Contains(fmt.Sprintf("%#v", result), token) {
			t.Fatalf("credential %q escaped: logs=%q result=%#v", token, logs.String(), result)
		}
	}
}

func TestActionsCacheRestoreAndSaveMintOnlyForMain(t *testing.T) {
	for _, operation := range []actionintegration.ActionsCacheOperation{actionintegration.ActionsCacheRestore, actionintegration.ActionsCacheSave} {
		t.Run(string(operation), func(t *testing.T) {
			workspace, _, job, materializer := actionsCacheRuntimeFixture(t, operation)
			marker := filepath.Join(workspace, "ran")
			node := writeActionsCacheFakeNode(t, workspace, `test -n "$ACTIONS_RUNTIME_TOKEN"; touch "$MARKER"`)
			job.Env = map[string]string{"MARKER": marker}
			tokens := &sequenceActionsCacheTokenSource{tokens: []string{string(operation) + "-token"}}
			redactor := &markerActionsCacheRedactor{dir: t.TempDir()}
			result, err := (Runner{Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: redactor}).RunJob(context.Background(), job, workspace)
			if err != nil || result.Conclusion != "success" || tokens.calls != 1 {
				t.Fatalf("RunJob() result = %#v, error = %v, token calls = %d", result, err, tokens.calls)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNestedCanonicalActionsCacheReceivesCredentialsOnlyInChild(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/cache.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested cache\n")
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: cache\nruns:\n  using: node24\n  main: main.js\n  post: post.js\n  post-if: always()\n")
	writeFixtureFile(t, remote, "main.js", "")
	writeFixtureFile(t, remote, "post.js", "")
	writeFixtureFile(t, remote, "parent/action.yml", `name: parent
runs:
  using: composite
  steps:
    - shell: sh
      run: test -z "${ACTIONS_RUNTIME_TOKEN-}"
    - uses: actions/cache@v4.2.0
    - shell: sh
      run: test -z "${ACTIONS_RUNTIME_TOKEN-}"
`)
	digest := digestTree(t, remote)
	parentID, cacheID := "a-0000000000000001", "a-0000000000000002"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "parent", Kind: "uses", Uses: "owner/repo/parent@main", Action: &plan.ActionSelector{Lock: parentID}}})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{
		{ID: parentID, Source: "github", Repository: "owner/repo", RequestedRef: "main", Commit: strings.Repeat("a", 40), Path: "parent", SourceDigest: digest, Children: map[string]plan.ActionSelector{"actions/cache@v4.2.0": {Lock: cacheID}}},
		{ID: cacheID, Source: "github", Repository: "actions/cache", RequestedRef: "v4.2.0", Commit: strings.Repeat("a", 40), SourceDigest: digest},
	}
	marker := filepath.Join(workspace, "cache-child")
	node := writeActionsCacheFakeNode(t, workspace, `test -n "$ACTIONS_RUNTIME_TOKEN"; touch "$CHILD_MARKER"`)
	job.Env = map[string]string{"CHILD_MARKER": marker}
	tokens := &sequenceActionsCacheTokenSource{tokens: []string{"nested-main-token", "nested-post-token"}}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	result, err := (Runner{Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: &markerActionsCacheRedactor{dir: t.TempDir()}}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || tokens.calls != 2 {
		t.Fatalf("RunJob() result = %#v, error = %v, token calls = %d", result, err, tokens.calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("nested cache child did not execute: %v", err)
	}
}

func TestActionsCacheSkippedStepDoesNotMint(t *testing.T) {
	workspace, _, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRoot)
	job.Steps[0].Condition = "false"
	missingNode := filepath.Join(workspace, "missing-node")
	tokens := &sequenceActionsCacheTokenSource{}
	result, err := (Runner{Node24: missingNode, Actions: materializer, ActionsCacheTokens: tokens, Redactor: &markerActionsCacheRedactor{dir: t.TempDir()}}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || tokens.calls != 0 {
		t.Fatalf("RunJob() result = %#v, error = %v, token calls = %d", result, err, tokens.calls)
	}
}

func TestActionsCacheRuntimeRevalidatesImmutableRefAndLifecycleBeforeMint(t *testing.T) {
	workspace, remote, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRoot)
	job.Actions[0].RequestedRef = "v3"
	job.Steps[0].Uses = "actions/cache@v3"
	tokens := &sequenceActionsCacheTokenSource{}
	if _, err := (Runner{Actions: materializer, ActionsCacheTokens: tokens}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "v4.2.0") {
		t.Fatalf("old immutable ref error = %v", err)
	}
	if materializer.calls != 0 || tokens.calls != 0 {
		t.Fatalf("old ref materialized/minted: %d / %d", materializer.calls, tokens.calls)
	}

	writeFixtureFile(t, remote, "action.yml", "name: cache\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n")
	digest := digestTree(t, remote)
	job.Actions[0].RequestedRef = "v4.2.0"
	job.Steps[0].Uses = "actions/cache@v4.2.0"
	job.Actions[0].SourceDigest = digest
	materializer.result.SourceDigest = digest
	if _, err := (Runner{Actions: materializer, ActionsCacheTokens: tokens}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "Node 20 or Node 24") {
		t.Fatalf("unexpected immutable lifecycle error = %v", err)
	}
	if tokens.calls != 0 {
		t.Fatalf("unexpected lifecycle minted %d tokens", tokens.calls)
	}
}

func TestActionsCacheRedactorFailurePreventsActionLaunchAndScrubsError(t *testing.T) {
	workspace, _, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRestore)
	marker := filepath.Join(workspace, "launched")
	node := writeActionsCacheFakeNode(t, workspace, `touch "$MARKER"`)
	job.Env = map[string]string{"MARKER": marker}
	token := "redactor-failure-token"
	tokens := &sequenceActionsCacheTokenSource{tokens: []string{token}}
	redactor := &markerActionsCacheRedactor{dir: t.TempDir(), err: fmt.Errorf("redactor echoed %s", token)}
	result, err := (Runner{Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: redactor}).RunJob(context.Background(), job, workspace)
	if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "external") && !strings.Contains(err.Error(), "redactor") || strings.Contains(err.Error(), token) {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("action launched despite redactor failure: %v", statErr)
	}
}

func TestActionsCacheRejectsTokenLeakageBeforeCommittingEffects(t *testing.T) {
	for _, channel := range []string{"output", "environment", "state", "path", "summary", "annotation", "error"} {
		t.Run(channel, func(t *testing.T) {
			workspace, _, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRestore)
			node := writeActionsCacheFakeNode(t, workspace, `
case "$LEAK_CHANNEL" in
  output) printf 'leak=%s\n' "$ACTIONS_RUNTIME_TOKEN" >> "$GITHUB_OUTPUT" ;;
  environment) printf 'LEAK=%s\n' "$ACTIONS_RUNTIME_TOKEN" >> "$GITHUB_ENV" ;;
  state) printf 'leak=%s\n' "$ACTIONS_RUNTIME_TOKEN" >> "$GITHUB_STATE" ;;
  path) printf '%s\n' "$ACTIONS_RUNTIME_TOKEN/bin" >> "$GITHUB_PATH" ;;
  summary) printf '%s\n' "$ACTIONS_RUNTIME_TOKEN" >> "$GITHUB_STEP_SUMMARY" ;;
  annotation) printf '::warning::%s\n' "$ACTIONS_RUNTIME_TOKEN" ;;
  error) printf '%s\n' "$ACTIONS_RUNTIME_TOKEN" >> "$GITHUB_OUTPUT" ;;
esac
`)
			job.Env = map[string]string{"LEAK_CHANNEL": channel}
			token := "leaked-" + channel + "-token"
			tokens := &sequenceActionsCacheTokenSource{tokens: []string{token}}
			var logs bytes.Buffer
			result, err := (Runner{Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: &markerActionsCacheRedactor{dir: t.TempDir()}, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
			if err == nil || result.Conclusion != "failure" || !strings.Contains(err.Error(), "token leakage detected") {
				t.Fatalf("RunJob() result = %#v, error = %v", result, err)
			}
			if strings.Contains(err.Error(), token) || strings.Contains(logs.String(), token) || strings.Contains(fmt.Sprintf("%#v", result), token) {
				t.Fatalf("token escaped rejection: error=%v logs=%q result=%#v", err, logs.String(), result)
			}
			if len(result.Outputs) != 0 || result.Env["LEAK"] != "" || len(result.State) != 0 || result.Summary != "" {
				t.Fatalf("leaked effects committed: %#v", result)
			}
		})
	}
}

func TestActionsCachePostUsesIndependentContextAfterMainCancellation(t *testing.T) {
	workspace, _, job, materializer := actionsCacheRuntimeFixture(t, actionintegration.ActionsCacheRoot)
	marker := filepath.Join(workspace, "post-ran")
	node := writeActionsCacheFakeNode(t, workspace, `
case "${1##*/}" in
  main.js) sleep 30 ;;
  post.js) touch "$POST_MARKER" ;;
esac
`)
	job.Env = map[string]string{"POST_MARKER": marker}
	tokens := &sequenceActionsCacheTokenSource{tokens: []string{"cancel-main-token", "cancel-post-token"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := (Runner{
		Node24: node, Actions: materializer, ActionsCacheTokens: tokens, Redactor: &markerActionsCacheRedactor{dir: t.TempDir()},
		InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond,
	}).RunJob(ctx, job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) || result.Conclusion != "cancelled" || tokens.calls != 2 {
		t.Fatalf("RunJob() result = %#v, error = %v, token calls = %d", result, err, tokens.calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache post did not run after cancellation: %v", err)
	}
}

func TestValidateActionsCachePlanRejectsBackgroundGraphAndJobContainer(t *testing.T) {
	cache := plan.ActionLock{ID: "cache", Source: "github", Repository: "actions/cache", RequestedRef: "v4.2.0"}
	parent := plan.ActionLock{ID: "parent", Source: "workspace", Children: map[string]plan.ActionSelector{"actions/cache@v4.2.0": {Lock: "cache"}}}
	job := plan.Job{
		Actions: []plan.ActionLock{parent, cache},
		Steps:   []plan.Step{{ID: "background", Kind: "uses", Background: true, Action: &plan.ActionSelector{Lock: "parent"}}},
	}
	if err := validateActionsCachePlan(job); err == nil || !strings.Contains(err.Error(), "background") {
		t.Fatalf("validateActionsCachePlan(background) = %v", err)
	}
	job.Steps[0].Background = false
	job.Container = &plan.Container{Image: "node:24"}
	if err := validateActionsCachePlan(job); err == nil || !strings.Contains(err.Error(), "job container") {
		t.Fatalf("validateActionsCachePlan(container) = %v", err)
	}
}
