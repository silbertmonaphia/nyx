package kb

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Candidate struct {
	Path    string
	RelPath string
	Size    int64
	ModTime time.Time
	DocType string
}

type ConnectorOptions struct {
	MaxFileBytes int64
	MaxFiles     int
	MaxDepth     int
}

//nolint:gocyclo // Each branch is one walk rule (ignore, depth, symlink, size, count).
func WalkSource(root string, opts ConnectorOptions) ([]Candidate, []ScanSkip, error) {
	realRoot, err := resolveDir(root)
	if err != nil {
		return nil, nil, err
	}
	var candidates []Candidate
	var skipped []ScanSkip
	err = filepath.WalkDir(realRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == realRoot {
				return walkErr
			}
			skipped = append(skipped, ScanSkip{RelPath: relPath(realRoot, path), Reason: "unreadable"})
			return nil
		}
		name := entry.Name()
		if path != realRoot && isIgnoredName(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel := relPath(realRoot, path)
		depth := pathDepth(rel)
		if opts.MaxDepth > 0 && depth > opts.MaxDepth {
			skipped = append(skipped, ScanSkip{RelPath: rel, Reason: "depth limit reached"})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		resolved, evalErr := filepath.EvalSymlinks(path)
		if evalErr != nil || !pathWithin(realRoot, resolved) {
			skipped = append(skipped, ScanSkip{RelPath: rel, Reason: "resolves outside the source root"})
			return nil
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.Mode().IsRegular() {
			skipped = append(skipped, ScanSkip{RelPath: rel, Reason: "not a regular file"})
			return nil
		}
		if opts.MaxFileBytes > 0 && info.Size() > opts.MaxFileBytes {
			skipped = append(skipped, ScanSkip{RelPath: rel, Reason: "file size limit exceeded"})
			return nil
		}
		if opts.MaxFiles > 0 && len(candidates) >= opts.MaxFiles {
			skipped = append(skipped, ScanSkip{RelPath: rel, Reason: "file count limit reached"})
			return fs.SkipAll
		}
		candidates = append(candidates, Candidate{
			Path:    resolved,
			RelPath: rel,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			DocType: docTypeForPath(rel),
		})
		return nil
	})
	if err != nil {
		return nil, skipped, err
	}
	return candidates, skipped, nil
}

func resolveDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", ErrInvalidSourceRoot
	}
	resolvedPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", ErrInvalidSourceRoot
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidSourceRoot
	}
	return resolvedPath, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isIgnoredName(name string) bool {
	return strings.HasPrefix(name, ".")
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func pathDepth(rel string) int {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return 0
	}
	return strings.Count(rel, "/")
}

func docTypeForPath(rel string) string {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".md", ".markdown":
		return DocTypeMarkdown
	default:
		return ""
	}
}
