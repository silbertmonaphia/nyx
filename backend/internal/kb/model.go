package kb

import (
	"time"
)

const (
	ScopeShared = "shared"
	ScopeUser   = "user"

	KindDir    = "dir"
	KindInline = "inline"

	OriginFile   = "file"
	OriginInline = "inline"

	StatusPending = "pending"
	StatusIndexed = "indexed"
	StatusFailed  = "failed"

	BlockText  = "text"
	BlockCode  = "code"
	BlockTable = "table"

	DocTypeMarkdown = "markdown"
)

type Source struct {
	ID         int64
	Scope      string
	OwnerID    int64
	Name       string
	Kind       string
	RootPath   string
	Enabled    bool
	LastScanAt *time.Time
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Document struct {
	ID          int64
	SourceID    int64
	Scope       string
	OwnerID     int64
	Origin      string
	RelPath     string
	Title       string
	DocType     string
	ContentHash []byte
	ByteSize    int64
	Mtime       *time.Time
	Status      string
	ErrorDetail string
	Generation  int32
	Content     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Chunk struct {
	ID             int64
	DocumentID     int64
	Scope          string
	OwnerID        int64
	SourceID       int64
	Generation     int32
	Ordinal        int
	HeadingPath    []string
	Content        string
	EmbedText      string
	ContentHash    []byte
	EmbeddingModel string
	TokenCount     int
	Locator        Locator
}

type Locator struct {
	Line          int `json:"line,omitempty"`
	EndLine       int `json:"end_line,omitempty"`
	TableRowStart int `json:"table_row_start,omitempty"`
	TableRowEnd   int `json:"table_row_end,omitempty"`
}

type Block struct {
	HeadingPath []string
	Kind        string
	Text        string
	Locator     Locator
	Table       *Table
}

type Table struct {
	Header []string
	Rows   [][]string
}

type NormalizedDocument struct {
	Title  string
	Blocks []Block
}

type Passage struct {
	ChunkID     int64
	DocID       int64
	SourceID    int64
	SourceName  string
	RelPath     string
	HeadingPath []string
	Content     string
	Locator     Locator
	Score       float64
}

type SourceStats struct {
	Documents int64
	Chunks    int64
	Pending   int64
}

type SourceInfo struct {
	Source
	Stats         SourceStats
	TopDirs       []string
	Indexed       bool
	EmbedModel    string
	IndexedModels []string
}

type OutlineSource struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Scope       string   `json:"scope"`
	Documents   int64    `json:"document_count"`
	Chunks      int64    `json:"chunk_count"`
	Directories []string `json:"directories"`
}

type Outline struct {
	Sources []OutlineSource `json:"sources"`
}

type ScanSkip struct {
	RelPath string
	Reason  string
}

type ScanResult struct {
	Documents int
	Chunks    int
	Skipped   []ScanSkip
}

type IndexReport struct {
	Sources []SourceScanResult
}

type SourceScanResult struct {
	SourceID int64
	Name     string
	Result   ScanResult
	Err      error
}

type ChunkOptions struct {
	TargetTokens  int
	MaxTokens     int
	MinTokens     int
	OverlapTokens int
}

type ChunkSpec struct {
	Ordinal     int
	HeadingPath []string
	Content     string
	EmbedText   string
	ContentHash []byte
	TokenCount  int
	Locator     Locator
}
