package enrich

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Options tunes the worker. Zero values fall back to sensible defaults.
type Options struct {
	Workers    int
	QueueSize  int
	SweepEvery time.Duration
}

// Worker enriches editions off the request path. Enqueue is non-blocking; the
// reconciler backstops anything dropped when the queue is full or missed across a
// restart. Jobs are keyed by ISBN and de-duplicated while in flight, so the same
// ISBN is never fetched twice concurrently.
type Worker struct {
	store      Store
	provider   Provider
	logger     *slog.Logger
	queue      chan string
	workers    int
	sweepEvery time.Duration

	mu       sync.Mutex
	inFlight map[string]bool
}

func NewWorker(store Store, provider Provider, logger *slog.Logger, opts Options) *Worker {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 128
	}
	if opts.SweepEvery <= 0 {
		opts.SweepEvery = 15 * time.Minute
	}
	return &Worker{
		store:      store,
		provider:   provider,
		logger:     logger,
		queue:      make(chan string, opts.QueueSize),
		workers:    opts.Workers,
		sweepEvery: opts.SweepEvery,
		inFlight:   make(map[string]bool),
	}
}

// Enqueue schedules an ISBN, coalescing duplicates and never blocking. If the
// queue is full the ISBN is dropped; the reconciler will pick it up on its next
// sweep, so nothing is permanently lost.
func (w *Worker) Enqueue(isbn string) {
	if isbn == "" || !w.markInFlight(isbn) {
		return
	}
	select {
	case w.queue <- isbn:
	default:
		w.unmarkInFlight(isbn)
		w.logger.Warn("enrich queue full; dropping, reconciler will retry", "isbn", isbn)
	}
}

// Run starts the worker goroutines and the reconciler, blocking until ctx is
// cancelled and the goroutines have drained.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for i := 0; i < w.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case isbn := <-w.queue:
					w.process(ctx, isbn)
					w.unmarkInFlight(isbn)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reconcile(ctx) // sweep once at startup, then periodically
		t := time.NewTicker(w.sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.reconcile(ctx)
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
}

// process enriches a single edition: skip if already done, otherwise look up the
// metadata and cover and persist them, marking the edition failed on error.
func (w *Worker) process(ctx context.Context, isbn string) {
	status, ok, err := w.store.EditionStatus(ctx, isbn)
	if err != nil {
		w.logger.Error("enrich: read edition status", "isbn", isbn, "error", err)
		return
	}
	if ok && status == StatusEnriched {
		return // another worker (or a previous run) already enriched this ISBN
	}

	meta, err := w.provider.Lookup(ctx, isbn)
	if err != nil {
		// Whether upstream has no record or the request failed, mark it failed so
		// it is visible; the reconciler may retry a transient failure later.
		if !errors.Is(err, ErrNotFound) {
			w.logger.Warn("enrich: lookup failed", "isbn", isbn, "error", err)
		}
		w.markFailed(ctx, isbn)
		return
	}

	// Save the cover (best effort) BEFORE flipping the edition to enriched, so the
	// "enriched" state is consistent: readers never see enriched metadata with the
	// cover still missing. A missing cover is not a failure.
	if cover, err := w.provider.FetchCover(ctx, isbn); err != nil {
		if !errors.Is(err, ErrNoCover) {
			w.logger.Warn("enrich: fetch cover", "isbn", isbn, "error", err)
		}
	} else if err := w.store.SaveCover(ctx, isbn, cover); err != nil {
		w.logger.Error("enrich: save cover", "isbn", isbn, "error", err)
	}

	if err := w.store.SaveEnrichment(ctx, isbn, meta, StatusEnriched); err != nil {
		w.logger.Error("enrich: save metadata", "isbn", isbn, "error", err)
	}
}

func (w *Worker) markFailed(ctx context.Context, isbn string) {
	if err := w.store.SaveEnrichment(ctx, isbn, nil, StatusFailed); err != nil {
		w.logger.Error("enrich: mark failed", "isbn", isbn, "error", err)
	}
}

func (w *Worker) reconcile(ctx context.Context) {
	isbns, err := w.store.PendingEnrichment(ctx, 100)
	if err != nil {
		w.logger.Error("enrich: reconcile sweep", "error", err)
		return
	}
	for _, isbn := range isbns {
		w.Enqueue(isbn)
	}
}

// markInFlight records isbn as queued/processing and returns false if it already
// was, so callers can skip enqueuing a duplicate.
func (w *Worker) markInFlight(isbn string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight[isbn] {
		return false
	}
	w.inFlight[isbn] = true
	return true
}

func (w *Worker) unmarkInFlight(isbn string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.inFlight, isbn)
}
