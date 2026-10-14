package mcp

// tools_list_contract_test.go freezes the MCP `tools/list` wire surface
// that the Python agent consumes. Two contracts live here:
//
//  1. Per-tool `annotations` — the data the agent's confirmation gate
//     keys off (openspec change `agent-feed-tools`, design D3): a
//     registry tool is treated as mutating unless its annotations
//     assert `readOnlyHint=true`, so the read-only claim has to be
//     pinned on the producer side.
//  2. The full native tool payload (`{name, description, inputSchema,
//     annotations}`) as a checked-in cross-runtime fixture (design D8)
//     at backend/agent_py/tests/fixtures/mcp_tools_list.json, asserted
//     against the live in-process response so Go-side drift fails CI
//     before it silently changes what the agent binds/gates.
//
// Regenerate the fixture after an intentional wire change with:
//
//	cd backend && go test ./internal/mcp/... -run TestHandler_ToolsList_GoldenFixture -update
//
// Without -update the test only asserts — CI never rewrites the
// contract.

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// updateFixtures regenerates the checked-in cross-runtime fixtures.
// Opt-in only: a plain `go test ./...` must fail on drift, not
// silently rewrite the artefact the Python side reads.
var updateFixtures = flag.Bool("update", false, "regenerate checked-in cross-runtime fixtures")

const (
	annotationReadOnlyHint    = "readOnlyHint"
	annotationDestructiveHint = "destructiveHint"
)

// toolsListFixturePath is the checked-in cross-runtime fixture,
// resolved relative to this package's directory (the test binary's
// working directory under `go test`). It holds the raw `result.tools`
// array — the JSON-RPC envelope is dropped because its `id` varies per
// request — in the MCP-native shape the agent's ToolsClient receives on
// the wire.
const toolsListFixturePath = "../../agent_py/tests/fixtures/mcp_tools_list.json"

// firstJSONObject returns the raw bytes of the first JSON object in an
// MCP HTTP response. The Streamable HTTP transport answers either with
// a bare application/json body or with an SSE stream whose first
// `data:` line carries the same envelope; the contract tests accept
// both (see decodeSSEOrJSONBody in handler_test.go).
func firstJSONObject(t *testing.T, body []byte) []byte {
	t.Helper()
	if json.Valid(body) {
		return body
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := []byte(strings.TrimPrefix(line, "data: "))
		if json.Valid(payload) {
			return payload
		}
	}
	t.Fatalf("could not decode MCP response body: %s", body)
	return nil
}

// liveToolsListRaw fires a real `tools/list` JSON-RPC request through
// the chi router (JWT middleware included) and returns result.tools
// exactly as the server emitted it.
func liveToolsListRaw(t *testing.T) json.RawMessage {
	t.Helper()

	stub := &stubFeedService{}
	svc := NewService(stub, noop.NewTracerProvider().Tracer("test"))
	h := NewHandler(svc, "/mcp", 64*1024)

	tokens := newTestTokens(t)
	router := newTestRouter(h, tokens, nil)

	rr := postRPCCall(t, router, jsonRPCRequest(MethodToolsList, 2, map[string]any{}),
		map[string]string{"Authorization": "Bearer " + bearerToken(t, tokens, 1, "tester")})
	require.Equal(t, http.StatusOK, rr.Code, "tools/list must succeed; body=%s", rr.Body.String())

	var envelope struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	body := firstJSONObject(t, rr.Body.Bytes())
	require.NoError(t, json.Unmarshal(body, &envelope), "tools/list must return a JSON-RPC envelope; body=%s", body)
	require.NotEmpty(t, envelope.Result.Tools, "tools/list result must carry a tools array")
	return envelope.Result.Tools
}

// liveToolsList returns the served tools keyed by name.
func liveToolsList(t *testing.T) map[string]map[string]any {
	t.Helper()

	var tools []map[string]any
	require.NoError(t, json.Unmarshal(liveToolsListRaw(t), &tools), "result.tools must be an array of objects")

	byName := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		require.NotEmpty(t, name, "every served tool must carry a name; got %v", tool)
		byName[name] = tool
	}
	return byName
}

// TestHandler_ToolsList_AnnotationContract pins the producer half of the
// confirmation-gate contract (design D3): read-only tools assert
// readOnlyHint=true AND destructiveHint=false (a read tool mislabeled
// destructive misleads hosts that surface the hint), mutating tools
// assert readOnlyHint=false AND destructiveHint=true. The consumer half lives in
// backend/agent_py/tests (normalization + gate), which fails safe when
// annotations are absent — so a dropped annotation here would silently
// turn every read into a confirmation prompt.
func TestHandler_ToolsList_AnnotationContract(t *testing.T) {
	tools := liveToolsList(t)
	require.Len(t, tools, 5, "the registry serves exactly the five feed tools")

	tests := []struct {
		tool            string
		wantReadOnly    bool
		wantDestructive *bool // nil → destructiveHint not asserted for this tool
	}{
		{tool: ToolListFeeds, wantReadOnly: true, wantDestructive: boolPtr(false)},
		{tool: ToolGetFeed, wantReadOnly: true, wantDestructive: boolPtr(false)},
		{tool: ToolCreateFeed, wantReadOnly: false, wantDestructive: boolPtr(true)},
		{tool: ToolUpdateFeed, wantReadOnly: false, wantDestructive: boolPtr(true)},
		{tool: ToolDeleteFeed, wantReadOnly: false, wantDestructive: boolPtr(true)},
	}

	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			raw, ok := tools[tc.tool]
			require.True(t, ok, "tool %q missing from tools/list", tc.tool)

			annotations, ok := raw["annotations"].(map[string]any)
			require.True(t, ok, "tool %q must carry an annotations object; got %T", tc.tool, raw["annotations"])

			readOnly, ok := annotations[annotationReadOnlyHint].(bool)
			require.True(t, ok, "tool %q must carry a boolean %s; got %v", tc.tool, annotationReadOnlyHint, annotations[annotationReadOnlyHint])
			assert.Equal(t, tc.wantReadOnly, readOnly,
				"tool %q %s must be %t — the agent's confirmation gate is fail-safe on absence, so this claim has to be explicit",
				tc.tool, annotationReadOnlyHint, tc.wantReadOnly)

			if tc.wantDestructive == nil {
				return
			}
			destructive, ok := annotations[annotationDestructiveHint].(bool)
			require.True(t, ok, "tool %q must carry a boolean %s; got %v", tc.tool, annotationDestructiveHint, annotations[annotationDestructiveHint])
			assert.Equal(t, *tc.wantDestructive, destructive,
				"tool %q %s must be %t", tc.tool, annotationDestructiveHint, *tc.wantDestructive)
		})
	}
}

// TestHandler_ToolsList_GoldenFixture asserts the live tools/list
// response deep-equals the checked-in cross-runtime fixture, and
// regenerates that fixture under `-update` (see the package comment
// above). Comparison is semantic (both sides decoded to `any`), so
// key order and whitespace in the file are presentation only.
func TestHandler_ToolsList_GoldenFixture(t *testing.T) {
	live := liveToolsListRaw(t)

	if *updateFixtures {
		var pretty bytes.Buffer
		require.NoError(t, json.Indent(&pretty, live, "", "  "), "result.tools must be valid JSON")
		pretty.WriteByte('\n')

		require.NoError(t, os.MkdirAll(filepath.Dir(toolsListFixturePath), 0o750))
		//nolint:gosec // G306: checked-in cross-runtime fixture, world-readable by design (see cmd/openapi)
		require.NoError(t, os.WriteFile(toolsListFixturePath, pretty.Bytes(), 0o644))
		t.Logf("regenerated %s", toolsListFixturePath)
	}

	fixtureBytes, err := os.ReadFile(toolsListFixturePath)
	require.NoError(t, err, "fixture missing — regenerate with: go test ./internal/mcp/... -run TestHandler_ToolsList_GoldenFixture -update")

	var want, got any
	require.NoError(t, json.Unmarshal(fixtureBytes, &want), "fixture must parse as JSON: %s", toolsListFixturePath)
	require.NoError(t, json.Unmarshal(live, &got), "live result.tools must be valid JSON")

	// The fixture is the captured tools array; assert the five tools and
	// their annotations explicitly so a fixture that lost its annotations
	// fails loudly instead of merely "differing".
	wantTools, ok := want.([]any)
	require.True(t, ok, "fixture root must be an array of tools; got %T", want)
	require.Len(t, wantTools, 5, "fixture must carry all five feed tools")
	for _, entry := range wantTools {
		toolObj, ok := entry.(map[string]any)
		require.True(t, ok, "fixture entries must be tool objects; got %T", entry)
		assert.NotEmpty(t, toolObj["name"], "fixture tool must carry a name")
		assert.NotNil(t, toolObj["description"], "fixture tool %v must carry a description", toolObj["name"])
		assert.NotNil(t, toolObj["inputSchema"], "fixture tool %v must carry an inputSchema", toolObj["name"])
		annotations, ok := toolObj["annotations"].(map[string]any)
		require.True(t, ok, "fixture tool %v must carry an annotations object", toolObj["name"])
		_, ok = annotations[annotationReadOnlyHint].(bool)
		assert.True(t, ok, "fixture tool %v must carry a boolean %s", toolObj["name"], annotationReadOnlyHint)
	}

	assert.Equal(t, want, got,
		"tools/list drifted from %s — if the wire change is intentional, regenerate with: go test ./internal/mcp/... -run TestHandler_ToolsList_GoldenFixture -update",
		toolsListFixturePath)
}

func boolPtr(b bool) *bool { return &b }
