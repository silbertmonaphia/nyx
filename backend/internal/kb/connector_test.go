package kb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func candidatePaths(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.RelPath)
	}
	return out
}

func TestWalkSourceIgnoresHiddenAndGit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "visible.md"), "visible")
	writeFile(t, filepath.Join(root, ".hidden.md"), "hidden")
	writeFile(t, filepath.Join(root, ".git", "config"), "git")
	writeFile(t, filepath.Join(root, ".DS_Store"), "junk")

	candidates, _, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 100, MaxDepth: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"visible.md"}, candidatePaths(candidates))
}

func TestWalkSourceEnforcesFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "small.md"), "1234567890")
	writeFile(t, filepath.Join(root, "big.md"), "123456789012345678901234567890")

	candidates, skipped, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 20, MaxFiles: 100, MaxDepth: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"small.md"}, candidatePaths(candidates))
	require.Len(t, skipped, 1)
	assert.Equal(t, "big.md", skipped[0].RelPath)
}

func TestWalkSourceEnforcesFileCountLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.md"), "a")
	writeFile(t, filepath.Join(root, "b.md"), "b")
	writeFile(t, filepath.Join(root, "c.md"), "c")

	candidates, skipped, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 2, MaxDepth: 10})
	require.NoError(t, err)
	assert.Len(t, candidates, 2)
	assert.NotEmpty(t, skipped)
}

func TestWalkSourceEnforcesDepthLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "root.md"), "root")
	writeFile(t, filepath.Join(root, "a", "one.md"), "one")
	writeFile(t, filepath.Join(root, "a", "b", "two.md"), "two")

	candidates, skipped, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 100, MaxDepth: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"a/one.md", "root.md"}, candidatePaths(candidates))
	assert.NotEmpty(t, skipped)
}

func TestWalkSourceRefusesEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.md")
	writeFile(t, outsideFile, "secret")
	writeFile(t, filepath.Join(root, "ok.md"), "ok")
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(root, "escape.md")))

	candidates, skipped, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 100, MaxDepth: 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"ok.md"}, candidatePaths(candidates))
	require.Len(t, skipped, 1)
	assert.Equal(t, "escape.md", skipped[0].RelPath)
	assert.Equal(t, "resolves outside the source root", skipped[0].Reason)
}

func TestWalkSourceAssignsDocType(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "note.md"), "# hi")
	writeFile(t, filepath.Join(root, "other.txt"), "plain")

	candidates, _, err := WalkSource(root, ConnectorOptions{MaxFileBytes: 1 << 20, MaxFiles: 100, MaxDepth: 10})
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.Equal(t, DocTypeMarkdown, candidates[0].DocType)
	assert.Equal(t, "", candidates[1].DocType)
}

func TestResolvePrivateRoot(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "user-a")
	require.NoError(t, os.MkdirAll(inside, 0o750))
	outside := t.TempDir()

	resolved, err := ResolvePrivateRoot(base, inside)
	require.NoError(t, err)
	assert.Equal(t, inside, resolved)

	_, err = ResolvePrivateRoot(base, base)
	assert.ErrorIs(t, err, ErrInvalidSourceRoot)

	_, err = ResolvePrivateRoot(base, outside)
	assert.ErrorIs(t, err, ErrInvalidSourceRoot)

	_, err = ResolvePrivateRoot(base, filepath.Join(base, "missing"))
	assert.ErrorIs(t, err, ErrInvalidSourceRoot)

	_, err = ResolvePrivateRoot("", inside)
	assert.ErrorIs(t, err, ErrPrivateBaseNotSetup)
}

func TestResolvePrivateRootRefusesSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(base, "user-a")
	require.NoError(t, os.Symlink(outside, link))

	_, err := ResolvePrivateRoot(base, link)
	assert.ErrorIs(t, err, ErrInvalidSourceRoot)
}
