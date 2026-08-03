package integration

import (
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

func TestLookupMatchesKnownCanonicalActions(t *testing.T) {
	tests := []struct {
		identity Identity
		want     Descriptor
	}{
		{Identity{Source: "github", Repository: "actions/checkout"}, Descriptor{Adapter: AdapterCheckoutExactEventSHA}},
		{Identity{Source: "github", Repository: "actions/cache"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "restore"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/cache", Path: "save"}, Descriptor{Service: ServiceCache}},
		{Identity{Source: "github", Repository: "actions/upload-artifact"}, Descriptor{Adapter: AdapterUploadArtifactBuildkite}},
		{Identity{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}, Descriptor{Service: ServiceArtifact}},
		{Identity{Source: "github", Repository: "actions/download-artifact"}, Descriptor{Adapter: AdapterDownloadArtifactBuildkite}},
	}
	for _, test := range tests {
		name := test.identity.Repository
		if test.identity.Path != "" {
			name += "/" + test.identity.Path
		}
		t.Run(name, func(t *testing.T) {
			got, ok := Lookup(test.identity)
			if !ok || got != test.want {
				t.Fatalf("Lookup(%#v) = %#v, %t, want %#v, true", test.identity, got, ok, test.want)
			}
		})
	}
}

func TestDownloadArtifactExactContract(t *testing.T) {
	if err := ValidateDownloadArtifactCommit(DownloadArtifactCommit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDownloadArtifactCommit(strings.Repeat("0", 40)); err == nil {
		t.Fatal("unrecognized commit accepted")
	}
	for _, inputs := range []map[string]string{{"name": "payload"}, {"Name": "payload", "path": "out", "merge-multiple": "False"}} {
		if err := ValidateDownloadArtifactInputs(inputs); err != nil {
			t.Fatalf("valid inputs rejected: %v", err)
		}
	}
	for name, inputs := range map[string]map[string]string{
		"all": nil, "expression": {"name": "${{ x }}"}, "absolute": {"name": "x", "path": "/tmp"},
		"merge": {"name": "x", "merge-multiple": "true"}, "ids": {"name": "x", "artifact-ids": "1"},
		"duplicate": {"name": "x", "Name": "y"},
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateDownloadArtifactInputs(inputs) == nil {
				t.Fatal("unsupported inputs accepted")
			}
		})
	}
}

func TestUploadArtifactCommitIsExact(t *testing.T) {
	if err := ValidateUploadArtifactCommit(UploadArtifactCommit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUploadArtifactCommit(strings.Repeat("0", 40)); err == nil {
		t.Fatal("unrecognized commit accepted")
	}
}

func TestValidateUploadArtifactInputs(t *testing.T) {
	for _, inputs := range []map[string]string{
		{"path": "payload/result.txt"},
		{
			"name": "payload-${{ matrix.variant }}", "path": "payload\nreports/result.txt\n",
			"if-no-files-found": "error", "include-hidden-files": "TRUE",
			"compression-level": "0", "overwrite": "False", "archive": "true",
		},
	} {
		if err := ValidateUploadArtifactInputs(inputs); err != nil {
			t.Fatalf("ValidateUploadArtifactInputs(%#v) = %v", inputs, err)
		}
	}

	rejected := map[string]map[string]string{
		"missing path":        nil,
		"glob":                {"path": "payload/**"},
		"path expression":     {"path": "${{ matrix.path }}"},
		"unclean path":        {"path": "./payload"},
		"too many roots":      {"path": strings.Repeat("payload\n", MaxUploadArtifactRoots+1)},
		"retention":           {"path": "payload", "retention-days": "1"},
		"overwrite":           {"path": "payload", "overwrite": "true"},
		"raw upload":          {"path": "payload", "archive": "false"},
		"bad no-files":        {"path": "payload", "if-no-files-found": "WARN"},
		"bad boolean":         {"path": "payload", "include-hidden-files": "1"},
		"bad compression":     {"path": "payload", "compression-level": "10"},
		"bad name":            {"path": "payload", "name": "bad/name"},
		"unknown":             {"path": "payload", "future-input": "value"},
		"case-colliding keys": {"path": "payload", "Name": "one", "name": "two"},
	}
	for name, inputs := range rejected {
		t.Run(name, func(t *testing.T) {
			if err := ValidateUploadArtifactInputs(inputs); err == nil {
				t.Fatalf("ValidateUploadArtifactInputs(%#v) succeeded", inputs)
			}
		})
	}
}

func TestLookupDoesNotBroadenCanonicalIdentity(t *testing.T) {
	for _, identity := range []Identity{
		{Source: "workspace", Repository: "actions/checkout"},
		{Source: "github", Repository: "Actions/Checkout"},
		{Source: "github", Repository: "actions/checkout", Path: "nested"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "actions/upload-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/download-artifact", Path: "nested"},
		{Source: "github", Repository: "owner/action"},
	} {
		if descriptor, ok := Lookup(identity); ok {
			t.Fatalf("Lookup(%#v) = %#v, true, want no integration", identity, descriptor)
		}
	}
}

func TestClassifyActionsCacheExactIdentity(t *testing.T) {
	for _, test := range []struct {
		identity  Identity
		operation ActionsCacheOperation
		ok        bool
	}{
		{identity: Identity{Source: "github", Repository: "actions/cache"}, operation: ActionsCacheRoot, ok: true},
		{identity: Identity{Source: "github", Repository: "actions/cache", Path: "restore"}, operation: ActionsCacheRestore, ok: true},
		{identity: Identity{Source: "github", Repository: "actions/cache", Path: "save"}, operation: ActionsCacheSave, ok: true},
		{identity: Identity{Source: "workspace", Repository: "actions/cache"}},
		{identity: Identity{Source: "github", Repository: "owner/cache"}},
		{identity: Identity{Source: "github", Repository: "actions/cache", Path: "nested"}},
	} {
		operation, ok := ClassifyActionsCache(test.identity)
		if operation != test.operation || ok != test.ok {
			t.Fatalf("ClassifyActionsCache(%#v) = %q, %t, want %q, %t", test.identity, operation, ok, test.operation, test.ok)
		}
	}
}

func TestValidateActionsCacheRequestedRef(t *testing.T) {
	for _, ref := range []string{"v4", "v4.2", "v4.2.0", "v4.2.1", "v5", "5.0.0", strings.Repeat("a", 40), "main", "arbitrary-branch", "releases/v3"} {
		if err := ValidateActionsCacheRequestedRef(ref); err != nil {
			t.Errorf("ValidateActionsCacheRequestedRef(%q) = %v", ref, err)
		}
	}
	for _, ref := range []string{"v0.9.0", "v1", "v2", "v3", "v3.9.9", "4.0.0", "v4.1", "v4.1.9", "v4.2.0-beta.1"} {
		if err := ValidateActionsCacheRequestedRef(ref); err == nil {
			t.Errorf("ValidateActionsCacheRequestedRef(%q) succeeded", ref)
		}
	}
}

func TestValidateActionsCacheLifecycle(t *testing.T) {
	validRoot := metadata.Runs{Using: "node20", Main: "dist/restore/index.js", Post: "dist/save/index.js", PostIf: "success()"}
	validLeaf := metadata.Runs{Using: "node24", Main: "dist/index.js"}
	for operation, runs := range map[ActionsCacheOperation]metadata.Runs{
		ActionsCacheRoot: validRoot, ActionsCacheRestore: validLeaf, ActionsCacheSave: validLeaf,
	} {
		if err := ValidateActionsCacheLifecycle(operation, runs); err != nil {
			t.Fatalf("ValidateActionsCacheLifecycle(%q) = %v", operation, err)
		}
	}
	for name, test := range map[string]struct {
		operation ActionsCacheOperation
		runs      metadata.Runs
	}{
		"unsupported runtime": {ActionsCacheRoot, metadata.Runs{Using: "composite", Main: "main.js", Post: "post.js"}},
		"pre":                 {ActionsCacheRoot, metadata.Runs{Using: "node20", Pre: "pre.js", Main: "main.js", Post: "post.js"}},
		"pre condition":       {ActionsCacheRoot, metadata.Runs{Using: "node20", PreIf: "always()", Main: "main.js", Post: "post.js"}},
		"missing main":        {ActionsCacheRoot, metadata.Runs{Using: "node20", Post: "post.js"}},
		"missing root post":   {ActionsCacheRoot, metadata.Runs{Using: "node20", Main: "main.js"}},
		"restore post":        {ActionsCacheRestore, metadata.Runs{Using: "node20", Main: "main.js", Post: "post.js"}},
		"save post condition": {ActionsCacheSave, metadata.Runs{Using: "node20", Main: "main.js", PostIf: "success()"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateActionsCacheLifecycle(test.operation, test.runs); err == nil {
				t.Fatal("unsupported lifecycle accepted")
			}
		})
	}
}

func TestValidateCheckoutInputs(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	for _, inputs := range []map[string]string{
		nil,
		{"repository": "BUILDKITE/BUILDKITE-GHA", "ref": sha, "fetch-depth": "1", "persist-credentials": "false", "clean": "true", "set-safe-directory": "true"},
	} {
		if err := ValidateCheckoutInputs(inputs, repository, sha); err != nil {
			t.Fatalf("ValidateCheckoutInputs(%#v) = %v", inputs, err)
		}
	}

	for name, inputs := range map[string]map[string]string{
		"token":                {"token": ""},
		"foreign repository":   {"repository": "other/repository"},
		"foreign ref":          {"ref": strings.Repeat("b", 40)},
		"submodules":           {"submodules": "true"},
		"path":                 {"path": "nested"},
		"persist credentials":  {"persist-credentials": "true"},
		"case-colliding names": {"Repository": repository, "repository": repository},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCheckoutInputs(inputs, repository, sha); err == nil || !strings.Contains(err.Error(), "Phase 6") {
				t.Fatalf("ValidateCheckoutInputs(%#v) = %v, want Phase 6 rejection", inputs, err)
			}
		})
	}
}
