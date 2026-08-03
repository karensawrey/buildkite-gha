# Development and release guide

## Set up the toolchain

The repository pins Go and all lint and release tools with mise:

```sh
mise trust mise.toml
mise install
```

Run the complete repository check with:

```sh
mise run check
```

`check` verifies formatting, builds the commands, runs standard and
race-enabled tests, runs `go vet`, golangci-lint, and shellcheck, validates the
signed plan-envelope fixtures, checks deterministic smoke compilation, and
validates the release configuration. `make check` is a convenience alias.
The standard and race-enabled suites run serially because their live container
tests inspect daemon-wide Docker resources. Local runs may skip those tests when
Docker or managed Node is unavailable; the hosted repository check requires the
live prerequisites and fails rather than silently losing Phase 5 coverage.

Focused tasks are also available:

```sh
mise run format
mise run build
mise run test
mise run test:race
mise run lint:go
mise run lint:shell
mise run vet
mise run plan-fixtures
```

## Understand the smoke lanes

The smoke inventory deliberately separates three kinds of evidence.

### Network-free compilation

```sh
mise run smoke:local
```

This validates the manifest and workflow syntax, then compiles each supported
fixture twice to check deterministic output. Expected-negative fixtures are
also checked. A pass is compile-time evidence only; it does not prove that a
workflow executes successfully.

See [`testdata/smoke/README.md`](../testdata/smoke/README.md) for the inventory
and the precise meaning of each expectation.

### Production-policy preflight

```sh
mise run smoke:profile
```

This opt-in networked lane anonymously resolves selected public actions and
applies the same `hosted-tokenless` admission policy as production `upload`.
It does not install Node or run action code. An `admitted` result is policy
evidence, not runtime evidence.

The exact audited `actions/upload-artifact` and exact-name
`actions/download-artifact` commits pass admission through bounded native
adapters. The canonical `actions/cache` root, restore, and save identities have
experimental admission for host execution, including through nested
composites; recognizable v1-v3 and pre-v4.2.0 refs, background use, and
job-container use fail admission. Artifact merge/broad download modes and
unsupported artifact commits also fail. Job and service container fixtures
have separate hosted runtime evidence but remain outside production admission.

Cache phases use the fixed `https://isaacsu-ghacs.buildkite.dev/` v2 results
service, mint a fresh token per phase, and are capped at 14 minutes. Hosted
agents must provide the upstream action's `tar` and `zstd` dependencies. The
runtime strips Node/process and proxy injection variables before launching a
credential-bearing phase; the service, rather than the runner, owns trust,
namespace, and read/write policy. See the compatibility guide for the complete
ref policy and security boundary.

### Hosted runtime proofs

Run all implemented hosted proofs against one exact commit:

```sh
commit=$(git rev-parse HEAD)
test ${#commit} -eq 40
bk build create --pipeline buildkite/buildkite-gha \
  --branch "$(git branch --show-current)" --commit "$commit" \
  --env SMOKE_PROBE=hosted --env SMOKE_COMMIT="$commit" --yes
```

The aggregate retains each phase's importer and continuation topology. Use a
phase selector only for targeted diagnosis:

| Coverage | Build environment |
| --- | --- |
| Sequential shell and upload | `PHASE2_PROBE=upload`, `PHASE2_COMMIT=<commit>` |
| Concurrent step controls | `PHASE3_PROBE=concurrent`, `PHASE3_COMMIT=<commit>` |
| Public JavaScript/composite actions | `PHASE4_PROBE=actions`, `PHASE4_COMMIT=<commit>` |
| Hosted Docker prerequisites | `PHASE5_PROBE=capabilities`, `PHASE5_COMMIT=<commit>` |
| Dockerfile action path | `PHASE5_PROBE=docker-action`, `PHASE5_COMMIT=<commit>` |
| Complete container runtime | `PHASE5_PROBE=runtime`, `PHASE5_COMMIT=<commit>` |
| Job summary annotation | `PHASE6_PROBE=summary`, `PHASE6_COMMIT=<commit>` |
| Workflow warning/error annotations | `PHASE6_PROBE=annotations`, `PHASE6_COMMIT=<commit>` |
| Upload-artifact publication | `PHASE6_PROBE=upload-artifact`, `PHASE6_COMMIT=<commit>` |
| Artifact producer/consumer roundtrip | `PHASE6_PROBE=artifact-roundtrip`, `PHASE6_COMMIT=<commit>` |

The cache preview proof remains separate from the network-free aggregate gate.
Run it on a hosted agent against one exact implementation commit and the real
preview service. Use two builds with the same unique cache key: the first must
miss and save from the root action's post phase, and the second must restore the
saved payload. The same proof must exercise `actions/cache/restore`,
`actions/cache/save`, a cache action nested in a composite, and cancellation
with an eligible root post. Independently inspect logs, outputs, state,
summaries, workflow-command annotations, and artifacts for both the minted
runtime token and `BUILDKITE_AGENT_ACCESS_TOKEN`. Record the exact commit,
builds, generated job IDs, and read-only observations before claiming hosted
runtime evidence; local fake-service tests are contract evidence only.

Summary annotation publication is advisory, so the generated job's successful
outcome does not by itself prove that Buildkite persisted the annotation. After
the targeted or aggregate build settles, use an authenticated `bk` CLI with
`read_builds` access to verify the job-scoped annotation independently:

```sh
scripts/phase-6-summary-annotation-verify <build-number> <commit>
```

The verifier reads only the generated job and its annotations. It requires
the build to match the expected exact commit and exactly one `info` annotation
with context `buildkite-gha-job-summary`, job scope, and both checked-in summary
fragments.

Workflow-command annotation publication is also advisory. Verify its distinct
warning and error contexts independently after the annotations proof settles:

```sh
scripts/phase-6-workflow-annotations-verify <build-number> <commit>
```

This verifier requires the generated job to pass even though it emitted an
`::error` command, then checks exactly one job-scoped `warning` annotation and
one job-scoped `error` annotation, their checked-in body fragments, and the
absence of the registered masking canary.

Artifact publication and consumption require independent native-storage
observations after their targeted or aggregate builds settle:

```sh
scripts/phase-6-upload-artifact-verify <build-number> <commit>
scripts/phase-6-artifact-roundtrip-verify <build-number> <commit>
```

The upload verifier binds the archive, terminal manifest, producer, digest,
contents, and action outputs. The roundtrip verifier additionally requires the
producer and both consumer matrix jobs to pass, checks all three terminal
manifests, and confirms both consumers observed the exact payload and compatible
absolute `download-path` output.

The phase definitions under `.buildkite/` and the [active
plan](plans/2026-07-22-buildkite-gha.md#current-progress) document the exact
coverage, differential oracles, commits, and historical Buildkite evidence.

## Architecture and plans

- [ADR 0001](architecture/0001-upstream-actions-reuse.md) explains why the
  compiler uses actionlint while act and the official runner remain behavioral
  references.
- [ADR 0002](architecture/0002-plan-envelope-trust-boundary.md) preserves the
  superseded Phase 0 signing experiment and its conformance history.
- The [active plan](plans/2026-07-22-buildkite-gha.md) tracks implementation
  phases, evidence, future UX, and deferred decisions. Current user-facing
  behavior belongs in the [README](../README.md) and [compatibility
  guide](compatibility.md), not only in the plan.

## Publish a release

From a clean, up-to-date `main`, run:

```sh
mise run release
```

The script runs `check`, chooses the next conventional-commit-derived v0 tag,
and pushes it. The tag webhook build reruns repository checks, creates the
GitHub release, and uploads the Linux x86-64 archive and checksum. Publication
can be retried from the same tag build; existing draft assets are replaced.

The source repository must be public before the first tag is pushed because the
companion plugin intentionally downloads release assets without a GitHub token.

### Release credential policy

The pipeline tag condition prevents accidental publication, but it is not a
secret boundary: pull request code can upload arbitrary Buildkite steps. Store
a fine-grained GitHub token with repository-scoped Contents read/write access as
the `GHA_GITHUB_RELEASE_TOKEN` Buildkite Secret, restricted to webhook-created
`v*` tag builds in the release pipeline:

```yaml
- pipeline_id: "019f8835-5873-4a64-850e-ba117a339d87"
  build_source: "webhook"
  build_branch: "v*"
```

Buildkite records a tag webhook's tag name as the build branch, so these claims
exclude ordinary branch and pull request webhook builds. The publisher also
verifies that the upstream tag, checked-out commit, and Buildkite commit agree
before retrieving the token. Never expose this token through a shared agent
environment hook.
