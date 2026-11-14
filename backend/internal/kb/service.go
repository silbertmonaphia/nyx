package kb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	catchUpBatch    = 10
	defaultDocLimit = 50
	maxDocLimit     = 100
	maxDocOffset    = 1_000_000
)

var inlineIndexLimit = 20 * time.Second

type Config struct {
	Enabled          bool
	Root             string
	SharedSourceName string
	Chunk            ChunkOptions
	TopK             int
	MaxContextChars  int
	IndexConcurrency int
}

type Settings struct {
	Enabled        bool   `json:"enabled"`
	EmbeddingModel string `json:"embedding_model"`
}

type Ingester interface {
	ScanSource(ctx context.Context, src Source) (ScanResult, error)
	IndexInline(ctx context.Context, doc Document) (int, error)
	FillDocument(ctx context.Context, doc Document) (int, error)
}

type Service struct {
	store              Store
	indexer            Ingester
	cfg                Config
	deploymentEmbedder Embedder
	embedderResolver   EmbedderResolver
	catchUp            sync.Map
	indexLocks         sync.Map
	documentLocks      sync.Map
	tracer             trace.Tracer
}

func NewService(store Store, indexer Ingester, cfg Config, deploymentEmbedder Embedder, tracer trace.Tracer) *Service {
	return &Service{
		store:              store,
		indexer:            indexer,
		cfg:                cfg,
		deploymentEmbedder: deploymentEmbedder,
		tracer:             tracer,
	}
}

func (s *Service) SetEmbedderResolver(fn EmbedderResolver) {
	s.embedderResolver = fn
}

func (s *Service) Enabled() bool {
	return s.cfg.Enabled
}

func (s *Service) ProvisionShared(ctx context.Context) (Source, error) {
	if strings.TrimSpace(s.cfg.Root) == "" {
		return Source{}, nil
	}
	root, err := resolveDir(s.cfg.Root)
	if err != nil {
		return Source{}, err
	}
	name := strings.TrimSpace(s.cfg.SharedSourceName)
	if name == "" {
		name = "shared"
	}
	return s.store.UpsertSharedSource(ctx, name, root)
}

func (s *Service) ProvisionInline(ctx context.Context, userID int64) (Source, error) {
	return s.store.EnsureInlineSource(ctx, userID)
}

func (s *Service) ListSources(ctx context.Context, userID int64) ([]SourceInfo, error) {
	sources, err := s.store.ListVisibleSources(ctx, userID)
	if err != nil {
		return nil, err
	}
	identity := s.embeddingIdentity(ctx, userID)
	out := make([]SourceInfo, 0, len(sources))
	for _, src := range sources {
		stats, statsErr := s.store.SourceStats(ctx, src.ID)
		if statsErr != nil {
			return nil, statsErr
		}
		info := SourceInfo{Source: src, Stats: stats, EmbedModel: identity}
		if src.Kind == KindDir {
			dirs, dirErr := s.store.ListTopLevelDirs(ctx, src.ID)
			if dirErr != nil {
				return nil, dirErr
			}
			info.TopDirs = dirs
		}
		models, modelErr := s.store.SourceEmbeddingModels(ctx, src.ID)
		if modelErr != nil {
			return nil, modelErr
		}
		info.IndexedModels = models
		info.Indexed = stats.Chunks > 0
		out = append(out, info)
	}
	return out, nil
}

func (s *Service) SetSourceEnabled(ctx context.Context, userID, sourceID int64, enabled bool) (Source, error) {
	src, err := s.store.GetSource(ctx, sourceID)
	if err != nil {
		return Source{}, err
	}
	if src.Scope == ScopeShared || src.OwnerID != userID {
		return Source{}, ErrSourceNotFound
	}
	return s.store.SetSourceEnabled(ctx, sourceID, userID, enabled)
}

func (s *Service) DeleteSource(ctx context.Context, userID, sourceID int64) error {
	src, err := s.store.GetSource(ctx, sourceID)
	if err != nil {
		return err
	}
	if src.Scope == ScopeShared {
		return ErrSharedSourceLocked
	}
	if src.OwnerID != userID {
		return ErrSourceNotFound
	}
	removed, err := s.store.DeleteUserSource(ctx, sourceID, userID)
	if err != nil {
		return err
	}
	if removed == 0 {
		return ErrSourceNotFound
	}
	return nil
}

func (s *Service) ListDocuments(ctx context.Context, userID int64, sourceID *int64, origin, status string, limit, offset int) ([]Document, error) {
	if limit <= 0 {
		limit = defaultDocLimit
	}
	if limit > maxDocLimit {
		limit = maxDocLimit
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxDocOffset {
		offset = maxDocOffset
	}
	//nolint:gosec // G115: limit is clamped to maxDocLimit and offset to maxDocOffset.
	return s.store.ListVisibleDocuments(ctx, userID, sourceID, origin, status, int32(limit), int32(offset))
}

func (s *Service) GetDocument(ctx context.Context, userID, documentID int64) (Document, error) {
	return s.store.GetVisibleDocument(ctx, documentID, userID)
}

func (s *Service) DocumentHeadings(ctx context.Context, userID, documentID int64) ([][]string, error) {
	doc, err := s.store.GetVisibleDocument(ctx, documentID, userID)
	if err != nil {
		return nil, err
	}
	paths, err := s.store.ListDocumentHeadingPaths(ctx, documentID, doc.Generation)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths, err = s.store.ListDocumentHeadingPaths(ctx, documentID, doc.Generation+1)
		if err != nil {
			return nil, err
		}
	}
	if paths == nil {
		paths = [][]string{}
	}
	return paths, nil
}

func (s *Service) CreateInlineDocument(ctx context.Context, userID int64, name, body string) (Document, error) {
	if strings.TrimSpace(body) == "" {
		return Document{}, ErrEmptyBody
	}
	relPath, err := sanitizeDocumentName(name)
	if err != nil {
		return Document{}, err
	}
	hash := hashBytes([]byte(body))
	src, err := s.store.EnsureInlineSource(ctx, userID)
	if err != nil {
		return Document{}, err
	}
	lock := s.sourceLock(src.ID)
	lock.Lock()
	defer lock.Unlock()
	if existing, lookupErr := s.store.GetInlineDocumentByHash(ctx, userID, hash); lookupErr == nil {
		return existing, nil
	}
	doc, err := s.store.UpsertInlineDocument(ctx, Document{
		SourceID:    src.ID,
		Scope:       ScopeUser,
		OwnerID:     userID,
		Origin:      OriginInline,
		RelPath:     relPath,
		Title:       titleFromPath(relPath),
		DocType:     DocTypeMarkdown,
		ContentHash: hash,
		ByteSize:    int64(len(body)),
		Content:     body,
	})
	if err != nil {
		return Document{}, err
	}
	s.indexInline(ctx, doc)
	s.ResetCatchUp(userID)
	return s.refreshDocument(ctx, doc), nil
}

func (s *Service) UpdateInlineDocument(ctx context.Context, userID, documentID int64, body string) (Document, error) {
	if strings.TrimSpace(body) == "" {
		return Document{}, ErrEmptyBody
	}
	lock := s.documentLock(documentID)
	lock.Lock()
	defer lock.Unlock()
	doc, err := s.store.GetOwnedDocument(ctx, documentID, userID)
	if err != nil {
		return Document{}, err
	}
	if doc.Origin == OriginFile {
		return Document{}, ErrFileBackedDocument
	}
	hash := hashBytes([]byte(body))
	if bytesEqual(doc.ContentHash, hash) {
		return doc, nil
	}
	updated, err := s.store.UpdateInlineDocumentContent(ctx, documentID, body, hash, int64(len(body)))
	if err != nil {
		return Document{}, err
	}
	s.indexInline(ctx, updated)
	s.ResetCatchUp(userID)
	return s.refreshDocument(ctx, updated), nil
}

func (s *Service) DeleteInlineDocument(ctx context.Context, userID, documentID int64) error {
	lock := s.documentLock(documentID)
	lock.Lock()
	defer lock.Unlock()
	doc, err := s.store.GetOwnedDocument(ctx, documentID, userID)
	if err != nil {
		return err
	}
	if doc.Origin == OriginFile {
		return ErrFileBackedDocument
	}
	return s.store.DeleteDocument(ctx, documentID)
}

func (s *Service) IndexVisible(ctx context.Context, userID int64) (IndexReport, error) {
	ctx, span := s.tracer.Start(ctx, "kb.index_visible", trace.WithAttributes(attribute.Int64("kb.user_id", userID)))
	defer span.End()

	sources, err := s.store.ListVisibleSources(ctx, userID)
	if err != nil {
		return IndexReport{}, err
	}
	report := IndexReport{Sources: make([]SourceScanResult, len(sources))}
	limit := s.cfg.IndexConcurrency
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for index, src := range sources {
		wg.Add(1)
		go func(index int, src Source) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result := SourceScanResult{SourceID: src.ID, Name: src.Name}
			scan, scanErr := s.IndexSource(ctx, src)
			result.Result = scan
			result.Err = scanErr
			report.Sources[index] = result
		}(index, src)
	}
	wg.Wait()
	return report, nil
}

func (s *Service) sourceLock(sourceID int64) *sync.Mutex {
	lock, _ := s.indexLocks.LoadOrStore(sourceID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) documentLock(documentID int64) *sync.Mutex {
	lock, _ := s.documentLocks.LoadOrStore(documentID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) IndexSource(ctx context.Context, src Source) (ScanResult, error) {
	lock := s.sourceLock(src.ID)
	lock.Lock()
	defer lock.Unlock()
	return s.indexer.ScanSource(ctx, src)
}

func (s *Service) SourceStats(ctx context.Context, src Source) (SourceStats, error) {
	return s.store.SourceStats(ctx, src.ID)
}

func (s *Service) ResetCatchUp(userID int64) {
	s.catchUp.Delete(userID)
}

func (s *Service) CatchUpOnce(ctx context.Context, userID int64) {
	once, _ := s.catchUp.LoadOrStore(userID, &sync.Once{})
	once.(*sync.Once).Do(func() {
		s.catchUpWork(ctx, userID)
	})
}

func (s *Service) catchUpWork(ctx context.Context, userID int64) {
	docs, err := s.store.ListPendingDocuments(ctx, userID, catchUpBatch)
	if err != nil {
		log.Warn().Err(err).Int64("user_id", userID).Msg("kb catch-up could not list pending documents")
		return
	}
	for _, doc := range docs {
		if _, fillErr := s.indexer.FillDocument(ctx, doc); fillErr != nil {
			log.Warn().Err(fillErr).Int64("document_id", doc.ID).Msg("kb catch-up fill failed")
		}
	}
	unembedded, err := s.store.ListDocumentsWithUnembedded(ctx, userID, catchUpBatch)
	if err != nil {
		log.Warn().Err(err).Int64("user_id", userID).Msg("kb catch-up could not list unembedded documents")
		return
	}
	for _, doc := range unembedded {
		if _, fillErr := s.indexer.FillDocument(ctx, doc); fillErr != nil {
			log.Warn().Err(fillErr).Int64("document_id", doc.ID).Msg("kb catch-up fill failed")
		}
	}
}

func (s *Service) Retrieve(ctx context.Context, userID int64, queryVec []float32, embeddingModel string, k int, sourceID *int64, pathPrefix string) ([]Passage, error) {
	if k <= 0 || len(queryVec) == 0 {
		return nil, nil
	}
	ctx, span := s.tracer.Start(ctx, "kb.retrieve",
		trace.WithAttributes(
			attribute.Int64("kb.user_id", userID),
			attribute.Int("kb.k", k),
		),
	)
	defer span.End()
	passages, err := s.store.RetrievePassages(ctx, RetrievalParams{
		Embedding:      queryVec,
		EmbeddingModel: embeddingModel,
		UserID:         userID,
		SourceID:       sourceID,
		PathPrefix:     pathPrefix,
		Limit:          k,
	})
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Int("kb.hits", len(passages)))
	return passages, nil
}

func (s *Service) Outline(ctx context.Context, userID int64) (Outline, error) {
	sources, err := s.store.ListVisibleSources(ctx, userID)
	if err != nil {
		return Outline{}, err
	}
	out := Outline{Sources: make([]OutlineSource, 0, len(sources))}
	for _, src := range sources {
		stats, statsErr := s.store.SourceStats(ctx, src.ID)
		if statsErr != nil {
			return Outline{}, statsErr
		}
		entry := OutlineSource{
			ID:          src.ID,
			Name:        src.Name,
			Scope:       src.Scope,
			Documents:   stats.Documents,
			Chunks:      stats.Chunks,
			Directories: []string{},
		}
		if src.Kind == KindDir {
			dirs, dirErr := s.store.ListTopLevelDirs(ctx, src.ID)
			if dirErr != nil {
				return Outline{}, dirErr
			}
			if dirs != nil {
				entry.Directories = dirs
			}
		}
		out.Sources = append(out.Sources, entry)
	}
	return out, nil
}

func (s *Service) Settings(ctx context.Context, userID int64) Settings {
	return Settings{
		Enabled:        s.cfg.Enabled,
		EmbeddingModel: s.embeddingIdentity(ctx, userID),
	}
}

func (s *Service) EmbeddingIdentity(ctx context.Context, userID int64) string {
	return s.embeddingIdentity(ctx, userID)
}

func (s *Service) embeddingIdentity(ctx context.Context, userID int64) string {
	embedder := s.resolveEmbedder(ctx, userID)
	if embedder == nil {
		return ""
	}
	return embedder.Model()
}

func (s *Service) resolveEmbedder(ctx context.Context, userID int64) Embedder {
	if s.embedderResolver != nil {
		embedder, err := s.embedderResolver(ctx, userID)
		if err != nil {
			log.Warn().Err(err).Int64("user_id", userID).Msg("kb per-user embedder unavailable")
			return nil
		}
		if embedder != nil {
			return embedder
		}
	}
	return s.deploymentEmbedder
}

func (s *Service) indexInline(ctx context.Context, doc Document) {
	indexCtx, cancel := context.WithTimeout(ctx, inlineIndexLimit)
	defer cancel()
	if _, err := s.indexer.IndexInline(indexCtx, doc); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn().Err(err).Int64("document_id", doc.ID).Msg("kb inline index failed")
	}
}

func (s *Service) refreshDocument(ctx context.Context, doc Document) Document {
	refreshed, err := s.store.GetDocument(ctx, doc.ID)
	if err != nil {
		return doc
	}
	return refreshed
}

func sanitizeDocumentName(name string) (string, error) {
	clean := strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if index := strings.LastIndex(clean, "/"); index >= 0 {
		clean = clean[index+1:]
	}
	if clean == "" || clean == "." || clean == ".." {
		return "", ErrInvalidDocName
	}
	if len([]rune(clean)) > 200 {
		return "", ErrInvalidDocName
	}
	lower := strings.ToLower(clean)
	if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".markdown") {
		clean += ".md"
	}
	return clean, nil
}
