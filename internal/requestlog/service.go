package requestlog

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/proxy"
)

// Service provides an async request log writer.
// EmitRequestLog performs a non-blocking channel send (drops on overflow).
// A background goroutine flushes batches to the Repo.
type Service struct {
	repo      *Repo
	queue     chan proxy.RequestLogEntry
	batchSize int
	interval  time.Duration
	flushReq  chan chan struct{}

	stopCh      chan struct{}
	wg          sync.WaitGroup
	dropped     atomic.Uint64
	flushFailed atomic.Bool
}

// ServiceConfig configures the request log service.
type ServiceConfig struct {
	Repo          *Repo
	QueueSize     int
	FlushBatch    int
	FlushInterval time.Duration
}

// NewService creates a new request log service.
func NewService(cfg ServiceConfig) *Service {
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = 8192
	}
	batchSize := cfg.FlushBatch
	if batchSize <= 0 {
		batchSize = 4096
	}
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Service{
		repo:      cfg.Repo,
		queue:     make(chan proxy.RequestLogEntry, queueSize),
		batchSize: batchSize,
		interval:  interval,
		flushReq:  make(chan chan struct{}, 64),
		stopCh:    make(chan struct{}),
	}
}

// Start launches the background flush goroutine.
func (s *Service) Start() {
	if s.repo != nil {
		s.repo.setReadBarrier(s.FlushNow)
	}
	s.wg.Add(1)
	go s.flushLoop()
}

// Stop signals the flush loop to stop, drains remaining entries, and returns.
func (s *Service) Stop() {
	if s.repo != nil {
		s.repo.setReadBarrier(nil)
	}
	close(s.stopCh)
	s.wg.Wait()
}

// EmitRequestLog enqueues a log entry. Non-blocking; drops on overflow.
func (s *Service) EmitRequestLog(entry proxy.RequestLogEntry) {
	select {
	case s.queue <- entry:
	default:
		// Queue full — drop entry to avoid blocking hot path.
		s.dropped.Add(1)
	}
}

// DroppedCount returns the number of entries dropped because the queue was full.
func (s *Service) DroppedCount() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// QueueDepth returns the number of entries waiting for the background flush.
func (s *Service) QueueDepth() int {
	if s == nil {
		return 0
	}
	return len(s.queue)
}

// QueueCapacity returns the configured non-blocking queue capacity.
func (s *Service) QueueCapacity() int {
	if s == nil {
		return 0
	}
	return cap(s.queue)
}

// FlushNow asks the background writer to flush current buffered data to DB,
// then blocks until that flush attempt completes.
func (s *Service) FlushNow() {
	done := make(chan struct{})
	select {
	case s.flushReq <- done:
	case <-s.stopCh:
		return
	}
	select {
	case <-done:
	case <-s.stopCh:
	}
}

// flushLoop runs until stopCh is closed, flushing on batch-size or timer.
func (s *Service) flushLoop() {
	defer s.wg.Done()

	batch := make([]proxy.RequestLogEntry, 0, s.batchSize)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	retryTicker := time.NewTicker(time.Second)
	defer retryTicker.Stop()
	pendingFailure := false

	for {
		if pendingFailure {
			// Keep the failed batch isolated while the repository is
			// unavailable. New entries remain in the bounded queue instead of
			// growing an unbounded in-memory retry slice.
			select {
			case <-retryTicker.C:
				if s.flush(batch) {
					batch = batch[:0]
					pendingFailure = false
				}
			case done := <-s.flushReq:
				batch = s.flushOnBarrier(batch, done)
				pendingFailure = len(batch) > 0 && s.flushFailed.Load()
			case <-s.stopCh:
				if len(batch) > 0 {
					_ = s.flush(batch)
				}
				return
			}
			continue
		}

		select {
		case entry := <-s.queue:
			batch = append(batch, entry)
			if len(batch) >= s.batchSize {
				if s.flush(batch) {
					batch = batch[:0]
				} else {
					pendingFailure = true
				}
			}

		case <-ticker.C:
			if len(batch) > 0 {
				if s.flush(batch) {
					batch = batch[:0]
				} else {
					pendingFailure = true
				}
			}

		case done := <-s.flushReq:
			batch = s.flushOnBarrier(batch, done)
			pendingFailure = len(batch) > 0 && s.flushFailed.Load()

		case <-s.stopCh:
			// Drain remaining.
			s.drainAndFlush(batch)
			return
		}
	}
}

func (s *Service) flushOnBarrier(batch []proxy.RequestLogEntry, firstWaiter chan struct{}) []proxy.RequestLogEntry {
	waiters := []chan struct{}{firstWaiter}
	for {
		select {
		case done := <-s.flushReq:
			waiters = append(waiters, done)
		default:
			goto flushed
		}
	}

flushed:
	if s.flushFailed.Load() {
		if len(batch) > 0 && s.flush(batch) {
			batch = batch[:0]
		}
		for _, done := range waiters {
			close(done)
		}
		return batch
	}

	// Bound barrier work to current queue depth snapshot so queries cannot be
	// blocked indefinitely by sustained write traffic.
	pending := len(s.queue)
drainLoop:
	for i := 0; i < pending; i++ {
		select {
		case entry := <-s.queue:
			batch = append(batch, entry)
			if len(batch) >= s.batchSize {
				if !s.flush(batch) {
					break drainLoop
				}
				batch = batch[:0]
			}
		default:
			break drainLoop
		}
	}
	if len(batch) > 0 && !s.flushFailed.Load() {
		if s.flush(batch) {
			batch = batch[:0]
		}
	}
	for _, done := range waiters {
		close(done)
	}
	return batch
}

func (s *Service) drainAndFlush(batch []proxy.RequestLogEntry) {
	for {
		select {
		case entry := <-s.queue:
			batch = append(batch, entry)
			if len(batch) >= s.batchSize {
				if !s.flush(batch) {
					return
				}
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				_ = s.flush(batch)
			}
			return
		}
	}
}

func (s *Service) flush(entries []proxy.RequestLogEntry) bool {
	if s.repo == nil {
		s.flushFailed.Store(true)
		log.Printf("[requestlog] flush %d entries failed: repository is nil", len(entries))
		return false
	}
	n, err := s.repo.InsertBatch(entries)
	if err != nil {
		s.flushFailed.Store(true)
		log.Printf("[requestlog] flush %d entries failed: %v", len(entries), err)
		return false
	}
	s.flushFailed.Store(false)
	if n > 0 {
		log.Printf("[requestlog] flushed %d entries", n)
	}
	return true
}

// Repo returns the underlying repository for query access.
func (s *Service) Repo() *Repo {
	return s.repo
}
