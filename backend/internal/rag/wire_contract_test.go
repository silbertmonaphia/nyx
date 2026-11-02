package rag

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nyx/internal/kb"
)

const pythonWireParser = `
import asyncio, json, sys
import httpx
from agent.rag_client import RagClient

raw = sys.stdin.buffer.read()

def handler(request):
    return httpx.Response(200, content=raw, headers={"content-type": "application/json"})

async def main():
    client = RagClient("http://test/internal/rag/retrieve", "token", transport=httpx.MockTransport(handler))
    result = await client.retrieve("hello", corpus="both")
    print(json.dumps({"context_block": result.context_block, "citations": result.citations}, ensure_ascii=False))

asyncio.run(main())
`

func pythonInterpreter(t *testing.T) string {
	t.Helper()
	venv := filepath.Join("..", "..", "..", "agent_py", ".venv", "bin", "python")
	if abs, err := filepath.Abs(venv); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			return abs
		}
	}
	for _, candidate := range []string{"python3", "python"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no python interpreter found for the cross-runtime wire test")
	return ""
}

const pythonOutlineParser = `
import asyncio, json, sys
import httpx
from agent.rag_client import RagClient

raw = sys.stdin.buffer.read()

def handler(request):
    return httpx.Response(200, content=raw, headers={"content-type": "application/json"})

async def main():
    client = RagClient("http://test/internal/rag/retrieve", "token", transport=httpx.MockTransport(handler))
    print(json.dumps(await client.outline(), ensure_ascii=False))

asyncio.run(main())
`

func runPythonWire(t *testing.T, script string, input []byte) []byte {
	t.Helper()
	python := pythonInterpreter(t)
	agentPath, err := filepath.Abs(filepath.Join("..", "..", "..", "agent_py"))
	require.NoError(t, err)
	env := append(os.Environ(), "PYTHONPATH="+agentPath)
	//nolint:gosec // G204: the interpreter path is resolved from a fixed allowlist and the probe is a test constant.
	probe := exec.Command(python, "-c", "import httpx, agent.rag_client")
	probe.Env = env
	if probe.Run() != nil {
		t.Skip("python agent client dependencies are unavailable for the cross-runtime wire test")
	}
	//nolint:gosec // G204: the interpreter path is resolved from a fixed allowlist and the script is a test constant.
	command := exec.Command(python, "-c", script)
	command.Env = env
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Run(), "python parser failed: %s", stderr.String())
	return stdout.Bytes()
}

func TestWireContractOutlineGoBytesParseInPython(t *testing.T) {
	outline := kb.Outline{Sources: []kb.OutlineSource{{
		ID:          7,
		Name:        "shared",
		Scope:       "shared",
		Documents:   12,
		Chunks:      43,
		Directories: []string{"B-Cook", "B-Sport"},
	}}}
	goBytes, err := json.Marshal(outline)
	require.NoError(t, err)

	output := runPythonWire(t, pythonOutlineParser, goBytes)
	var sources []map[string]any
	require.NoError(t, json.Unmarshal(output, &sources))
	require.Len(t, sources, 1)
	assert.Equal(t, float64(7), sources[0]["id"])
	assert.Equal(t, "shared", sources[0]["name"])
	assert.Equal(t, "shared", sources[0]["scope"])
	assert.Equal(t, float64(12), sources[0]["document_count"])
	assert.Equal(t, float64(43), sources[0]["chunk_count"])
	assert.Equal(t, []any{"B-Cook", "B-Sport"}, sources[0]["directories"])
}

func TestWireContractGoBytesParseInPython(t *testing.T) {
	service, _, _ := newCorpusService(t,
		[]Passage{{FeedID: 1, Title: "Feed title", Description: "feed description", Score: 0.4}},
		[]kb.Passage{{
			DocID:       7,
			SourceName:  "shared",
			RelPath:     "B-Cook/diet.md",
			HeadingPath: []string{"煮食记", "碳水列表"},
			Content:     "一份碳水 + 一份蛋白",
			Locator:     kb.Locator{Line: 12, EndLine: 14},
			Score:       0.9,
		}},
	)
	recorder := invokeHandler(t, Handler(service), "/internal/rag/retrieve?query=hello&corpus=both", 42)
	require.Equal(t, http.StatusOK, recorder.Code)
	goBytes := recorder.Body.Bytes()

	python := pythonInterpreter(t)
	agentPath, err := filepath.Abs(filepath.Join("..", "..", "..", "agent_py"))
	require.NoError(t, err)
	env := append(os.Environ(), "PYTHONPATH="+agentPath)
	//nolint:gosec // G204: the interpreter path is resolved from a fixed allowlist and the probe is a test constant.
	probe := exec.Command(python, "-c", "import httpx, agent.rag_client")
	probe.Env = env
	if probe.Run() != nil {
		t.Skip("python agent client dependencies are unavailable for the cross-runtime wire test")
	}
	//nolint:gosec // G204: the interpreter path is resolved from a fixed allowlist and the script is a test constant.
	command := exec.Command(python, "-c", pythonWireParser)
	command.Env = env
	command.Stdin = bytes.NewReader(goBytes)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	require.NoError(t, command.Run(), "python parser failed: %s", stderr.String())

	var envelope struct {
		ContextBlock string             `json:"context_block"`
		Citations    []retrieveCitation `json:"citations"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	require.Len(t, envelope.Citations, 2)

	var feedSeen, docSeen bool
	for _, citation := range envelope.Citations {
		switch citation.Kind {
		case CitationKindFeed:
			feedSeen = true
			assert.Equal(t, int64(1), citation.FeedID)
			assert.Equal(t, "Feed title\n\nfeed description", citation.Snippet)
		case CitationKindDoc:
			docSeen = true
			assert.Equal(t, int64(0), citation.FeedID)
			assert.Equal(t, int64(7), citation.DocID)
			assert.Equal(t, "shared", citation.SourceName)
			assert.Equal(t, "B-Cook/diet.md", citation.RelPath)
			assert.Equal(t, []string{"煮食记", "碳水列表"}, citation.HeadingPath)
			assert.Equal(t, "一份碳水 + 一份蛋白", citation.Snippet)
		}
	}
	assert.True(t, feedSeen)
	assert.True(t, docSeen)
	assert.Contains(t, envelope.ContextBlock, "B-Cook/diet.md")
}
