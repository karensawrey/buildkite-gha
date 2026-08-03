# Upstream `actions/cache` support through the Buildkite GHA cache service

Status: **Ready for implementation**
Date: 2026-08-03
Target repository: `buildkite/buildkite-gha`

## Summary

Add narrowly scoped support for the canonical GitHub `actions/cache` action by
executing its upstream JavaScript with credentials for Buildkite's
GitHub-Actions-compatible cache service. The parent `buildkite-gha` process
mints a short-lived runtime token from the current Buildkite job's agent
credential immediately before each cache lifecycle phase. The agent credential
must never enter a workflow or action subprocess.

The cache action receives exactly this service configuration:

```text
ACTIONS_RESULTS_URL=https://isaacsu-ghacs.buildkite.dev/
ACTIONS_CACHE_SERVICE_V2=true
ACTIONS_RUNTIME_TOKEN=<freshly minted token>
```

`ACTIONS_CACHE_URL` and `ACTIONS_RUNTIME_URL` are deliberately absent. The
upstream action continues to own cache key handling, archive behavior, outputs,
and best-effort service-error semantics.

```text
verified canonical action lock
              |
              v
parent buildkite-gha process reads BUILDKITE_JOB_ID and agent token
              |
              v
POST https://agent.buildkite.com/v3/jobs/{job-id}/ghac_tokens
              |
              v
register minted token with both redactors
              |
              v
invoke only the actions/cache Node phase with protected environment
```

## Settled external contracts

### Token minting

The runner sends:

```http
POST /v3/jobs/{BUILDKITE_JOB_ID}/ghac_tokens HTTP/1.1
Host: agent.buildkite.com
Authorization: Token {BUILDKITE_AGENT_ACCESS_TOKEN}
Content-Type: application/json
Accept: application/json

{}
```

The full production endpoint is hardcoded for this increment:

```text
https://agent.buildkite.com/v3/jobs/{BUILDKITE_JOB_ID}/ghac_tokens
```

Do not honor `BUILDKITE_AGENT_ENDPOINT` yet. `BUILDKITE_JOB_ID` is required to
be a UUID and must be validated before URL construction.

A successful response is HTTP 200 with a JSON content type and a token at
`.token`:

```json
{"token":"..."}
```

Additional response fields are allowed. The token must be non-empty and no
larger than 64 KiB. Bound the entire response body independently.

Minted tokens:

- are valid for 15 minutes;
- permit both cache reads and writes;
- are authorized and namespace-scoped by the token minter/cache service;
- are minted afresh for every executed cache lifecycle phase; and
- may be minted during post cleanup after the main job context is cancelled.

The runner does not implement trusted/untrusted branch policy. The token minter
and cache service enforce all read/write and namespace policy from the
authenticated Buildkite job identity.

### Mint retries

Use at most three total attempts under one 30-second deadline. Retry only:

- connection failures;
- timeouts while the overall context remains live;
- HTTP 429; and
- HTTP 5xx.

Fail all other HTTP 4xx responses immediately. Use bounded exponential backoff
with jitter. Honor both forms of the standard `Retry-After` header, but never
sleep beyond the shared deadline.

The mint client must disable redirects and ambient proxy use. Errors may report
the HTTP status and a safe request-ID header, if present, but must not include
the response body, agent token, or minted token.

### Results service

The protected service values are:

```text
ACTIONS_RESULTS_URL=https://isaacsu-ghacs.buildkite.dev/
ACTIONS_CACHE_SERVICE_V2=true
```

They are fixed in the runtime, not configurable by workflow, plan, CLI, or
environment. The action receives a newly minted `ACTIONS_RUNTIME_TOKEN` beside
them.

## Supported action surface

Support only these canonical identities:

```yaml
- uses: actions/cache@v4.2.0
- uses: actions/cache/restore@v4.2.0
- uses: actions/cache/save@v4.2.0
```

The same identities are supported when reached transitively through a verified
composite action's immutable child locks.

Version policy:

- v4.2.0 is the documented and tested minimum;
- reject requested refs that clearly denote v1-v3 or a semantic version below
  v4.2.0;
- accept moving major tags, literal SHAs, `main`, arbitrary branches, and other
  opaque refs for now; and
- do not maintain a commit allowlist in this increment.

The compiler and runtime still bind and verify the exact resolved commit and
source-tree digest. Trusting arbitrary canonical repository commits is an
explicit initial tradeoff, not an accidental broadening.

Do not provision credentials for:

- forks;
- workspace actions that resemble `actions/cache`;
- unknown paths under the canonical repository;
- third-party actions that import the `@actions/cache` npm package internally;
- background cache steps;
- background composite steps whose transitive action tree contains cache; or
- actions/cache running inside a job container.

The cache action must declare a JavaScript runtime supported by this runner:
Node 20 or Node 24. Fail closed if canonical cache metadata unexpectedly:

- declares a pre phase;
- changes to composite or Docker execution; or
- gives `/restore` or `/save` a post phase.

The root action may use its expected main restore plus post save lifecycle.

## Compiler and admission changes

The integration catalog in `internal/action/integration/integration.go` already
classifies the root, restore, and save identities as `ServiceCache`. Extend that
classification with helpers for:

- exact canonical cache identity;
- recognizable minimum-version validation;
- root/restore/save operation classification; and
- allowed lifecycle shape.

Keep cache classified as a service, not an adapter: its upstream JavaScript
must pass through normal action execution.

Apply identity and recognizable-version validation while constructing action
locks in `internal/compiler/actions.go`. Validate again from the immutable lock
in `internal/runtime/actions.go`; a manually constructed plan must not bypass
the boundary.

After action-lock construction, walk the selector/child-lock graph for every
background step. Reject a background step if any reachable lock is canonical
cache. This covers nested composite use rather than only direct workflow
references.

Update `validateUnprivilegedBundle` in `internal/cli/cli.go` to admit
`ServiceCache` only for the three canonical identities after all cache checks
pass. Continue rejecting every other unavailable GitHub Actions service.

Do not add a capability or change the job-plan schema in this increment. The
verified immutable action lock is the runtime gate. Record a dedicated
`actions-cache` capability and a new immutable schema version as a future
hardening option.

General `run-job` execution supports cache whenever it has a valid verified
plan and Buildkite job credentials. Support is not restricted to plans produced
by the `hosted-tokenless` upload profile and has no feature gate.

## Parent-owned token minter

Add a small injectable runtime interface, conceptually:

```go
type ActionsCacheTokenSource interface {
	Mint(context.Context) (string, error)
}
```

Use the smallest shape required by the fixed empty request. The service does
not distinguish read from write at mint time.

The CLI installs the production implementation when constructing
`runtime.Runner`. Tests inject a fake source. The production source reads
`BUILDKITE_JOB_ID` and `BUILDKITE_AGENT_ACCESS_TOKEN` lazily when a cache phase
actually executes. Do not read or mint during compilation, plan validation,
remote action verification, skipped steps, or skipped post conditions.

Use a dedicated `http.Client`/transport with:

- a fixed HTTPS origin;
- no proxy function;
- redirects rejected before a second request is sent;
- context cancellation;
- the shared 30-second mint deadline;
- a bounded response body; and
- strict single-document JSON parsing while tolerating unknown fields.

Do not add endpoint configuration solely for tests. Test the production URL and
headers with an injected `RoundTripper`, and test runner behavior through the
token-source interface.

## Runtime lifecycle integration

Carry a private trusted cache classification on a resolved JavaScript action or
prepared invocation. Populate it only from the already verified action lock.
Do not infer privilege from the user-facing `uses` string at process launch.

Make the JavaScript lifecycle call identify the logical phase explicitly
(`pre`, `main`, or `post`) instead of inferring it solely from an entrypoint
filename. This allows lifecycle validation and per-phase minting even if two
metadata fields happen to name the same file.

Immediately before every eligible main or post phase:

1. Reconfirm canonical cache classification and expected lifecycle.
2. Reject job-container execution.
3. Mint a fresh token under the 30-second mint policy.
4. Register the token with both redactors.
5. Build and sanitize the final phase environment.
6. Start a phase context capped at 14 minutes.
7. Invoke the existing upstream Node entrypoint.
8. Inspect command-file effects for credential leakage before committing them.

The 14-minute cap leaves a one-minute margin before the token's 15-minute
expiry. It applies independently to root main, root post, restore main, and save
main, and is further shortened by any active outer step deadline.

For cache post phases, create an independent background-derived cleanup context
so cleanup remains possible after main cancellation. Give each cache post its
own 14-minute cap. Preserve the existing cleanup behavior for non-cache posts.
Evaluate the upstream post condition before minting.

Root main and root post must receive different freshly minted tokens. Never
store a token in `preparedInvocation.state`, action state, job environment, or
the post registry's serializable effects.

## Protected process environment

Build the normal action environment first, including job/step environment,
inputs, action path, state, command-file paths, and prior legitimate effects.
Then sanitize and overlay the protected service values last.

Remove all workflow-provided values for:

```text
ACTIONS_RESULTS_URL
ACTIONS_RUNTIME_TOKEN
ACTIONS_CACHE_SERVICE_V2
ACTIONS_CACHE_URL
ACTIONS_RUNTIME_URL
```

Set only the first three to the runtime-owned values and leave the latter two
absent.

Remove these process-injection variables from credential-bearing cache phases:

```text
NODE_OPTIONS
NODE_PATH
NODE_EXTRA_CA_CERTS
NODE_TLS_REJECT_UNAUTHORIZED
SSLKEYLOGFILE
LD_PRELOAD
LD_LIBRARY_PATH
```

Also remove workflow-controlled proxy variables:

```text
HTTP_PROXY
HTTPS_PROXY
ALL_PROXY
NO_PROXY
http_proxy
https_proxy
all_proxy
no_proxy
```

Apply deletion to the final merged environment so a prior `GITHUB_ENV` effect
cannot reintroduce a value. Keep normal cache configuration such as
`SEGMENT_DOWNLOAD_TIMEOUT_MINS`.

The runner's existing `processEnv` allowlist remains the ambient environment
boundary. Never add `BUILDKITE_AGENT_ACCESS_TOKEN` or `BUILDKITE_JOB_ID` to
child-process allowlists or standard job environment.

## Redaction and leakage containment

Before Node starts, register the minted token with:

1. the in-process command processor mask; and
2. `buildkite-agent redactor add` through the configured `Runner.Redactor`.

If external redactor registration fails, fail the cache phase without launching
Node. Never print the redactor command or token in an error.

The upstream action necessarily receives the runtime token. Prevent it from
propagating the literal value to later workflow execution. Before committing
phase effects, detect the token in:

- outputs;
- environment changes;
- state;
- PATH entries;
- summaries;
- workflow-command annotations; and
- returned errors.

Scrub/remove any occurrence and fail the phase. This check must happen before
`commitStepExecution`, not only during final job-result scrubbing, so sibling
steps cannot inherit the credential through `GITHUB_ENV` or related files.
Existing final result scrubbing remains a defense in depth.

The parent agent token is never passed to the action and must not be included in
HTTP errors. The mint client disables redirects and proxies specifically to
avoid forwarding it outside the fixed origin.

## Upstream semantics and tools

After successful provisioning, preserve upstream action behavior:

- cache keys and restore keys;
- `cache-hit` and other action outputs;
- archive creation and extraction;
- root post-save registration and LIFO ordering;
- upstream warnings and best-effort handling of cache-service failures; and
- composite action state/output behavior.

Minting and redactor failures are hard step failures. Results-service failures
after Node starts retain upstream conclusions.

Rely on the agent image for `tar`, `zstd`, and other tools used by the action.
Do not install, pin, or manage them in this change. Missing tools produce the
upstream action's normal error.

## Tests

### Integration and compiler

- Recognize only canonical root, restore, and save identities.
- Accept v4.2.0 and clearly newer semantic refs.
- Reject v1-v3 and semantic refs below v4.2.0.
- Accept literal SHAs, moving major tags, `main`, and arbitrary branches.
- Exclude forks, workspace lookalikes, unknown paths, and third-party toolkit
  consumers.
- Detect direct and composite-transitive cache locks.
- Reject direct background cache steps.
- Reject background composites containing cache at any supported nesting depth.
- Reject cache combined with a job container.
- Reject unexpected pre, restore/save post, composite, Docker, and unsupported
  Node metadata.
- Preserve admission rejection for every non-cache service action.

### Mint client

- Verify exact method, URL, body, headers, and `Token` authentication.
- Validate job UUID before network activity.
- Fail safely when either required ambient variable is missing.
- Require HTTP 200 and JSON content type.
- Accept unknown JSON fields.
- Reject missing, empty, oversized, malformed, trailing, and oversized-body
  responses.
- Reject redirects without sending a second authenticated request.
- Prove ambient proxies are ignored.
- Retry network failures, 429, and 5xx up to three total attempts.
- Do not retry other 4xx responses.
- Honor bounded `Retry-After` and the shared 30-second deadline.
- Stop retrying when the parent context is cancelled.
- Prove errors omit authorization, token, and response body.

### Runtime and lifecycle

- Root main and post each mint once and receive different tokens.
- Restore and save each mint once for main.
- Skipped steps and skipped post conditions never mint.
- Minting starts only after immutable action verification.
- Cache phases receive exactly the three protected service values.
- Workflow values cannot override protected values.
- Legacy cache/runtime URLs remain absent.
- Parent composites, siblings, shell steps, and ordinary actions receive no
  runtime token.
- Nested canonical cache receives credentials.
- Every listed Node/process/proxy injection variable is removed from the final
  environment, including values introduced through prior environment effects.
- Normal cache settings remain available.
- Agent token and job ID remain absent from all child environments.
- In-process and external redaction happen before Node starts.
- Redactor failure prevents action execution.
- Literal token leakage through every command-file/result channel is rejected
  before effects are committed.
- Root post can mint and execute after main cancellation when its upstream
  condition permits.
- Each phase is independently capped at 14 minutes.
- Non-cache post timeout behavior is unchanged.
- Job-container execution fails before minting.
- Upstream cache outputs and error conclusions remain unchanged.

Run relevant tests with `go test -race` because post registration and action
execution have concurrency-aware code paths.

### Hosted proof

Keep the default CI and `mise run check` network-free. Document and perform a
separate hosted proof against the real preview service:

1. First build misses, then root post saves.
2. Second build restores the saved cache.
3. Exercise root, restore-only, and save-only forms.
4. Exercise cache nested in a composite action.
5. Exercise cancellation and eligible post cleanup.
6. Confirm logs, outputs, state, summaries, annotations, and artifacts contain
   neither credential.

## Documentation changes

Update `README.md`, `docs/compatibility.md`, `docs/development.md`, and the smoke
fixture documentation to replace the blanket cache rejection with an
experimental support statement. Document:

- canonical `actions/cache` only;
- fixed v2 results service URL;
- v4.2.0 tested minimum and the permissive no-allowlist ref policy;
- root, restore, save, and nested-composite support;
- no background or job-container support;
- 14-minute phase limit;
- image-provided `tar`/`zstd` requirement;
- stripped process/proxy environment behavior;
- server-owned trust, namespace, and read/write policy; and
- the optional future `actions-cache` capability/schema hardening.

## File-level implementation map

- `internal/action/integration/integration.go`: identity, operation, version,
  and lifecycle helpers.
- `internal/action/integration/integration_test.go`: exact identity and version
  boundaries.
- `internal/compiler/actions.go`: compile-time cache validation and transitive
  lock evidence.
- `internal/compiler/compiler.go`: reject direct/transitive background use and
  cache plus job containers.
- `internal/compiler/actions_test.go` and `compiler_test.go`: plan and graph
  cases.
- `internal/cli/cli.go`: admit the exact cache service integration and wire the
  production token source into `runtime.Runner`.
- `internal/cli/cli_test.go`: hosted-profile admission and runtime wiring.
- `internal/runtime/actions.go`: runtime lock revalidation and trusted cache
  classification.
- `internal/runtime/job.go`: lifecycle phase identity, per-phase minting,
  independent cache post context, and registration flow.
- `internal/runtime/runtime.go`: protected environment construction,
  sanitization, 14-minute execution cap, and pre-commit leakage checks.
- A focused runtime token-source file if keeping HTTP protocol code out of the
  already large runtime files materially improves ownership; do not create a
  generic credential framework.
- Runtime tests: token HTTP contract, phase behavior, redaction, environment,
  cancellation, and leakage containment.
- User and developer documentation listed above.

No serialized plan fields, schema documents, generated pipeline environment,
or transport result formats need to change.

## Explicitly deferred hardening

- Dedicated `actions-cache` plan capability and a new immutable schema version.
- Commit/release allowlisting for credential-bearing canonical action source.
- Production replacement for the preview results-service URL.
- Respecting a trusted `BUILDKITE_AGENT_ENDPOINT` rather than the fixed mint
  origin.
- Job-container support.
- A trusted administrator proxy/custom-CA channel distinct from workflow env.
- Token caching or expiry-aware reuse; per-phase minting is intentional now.
- Third-party actions that directly consume the cache toolkit.

These are not prerequisites for this agreed initial scope, but the first two
should be reconsidered before broad production rollout.
