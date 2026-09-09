package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"obsigon/internal/rag"
)

type IndexProgress struct {
	mu          sync.RWMutex
	running     bool
	phase       string
	processed   int
	total       int
	currentFile string
	startedAt   *time.Time
	finishedAt  *time.Time
	result      any
	errorText   *string
	cancel      context.CancelFunc
	cancelled   bool
}

func NewIndexProgress() *IndexProgress { return &IndexProgress{phase: "idle"} }

func (p *IndexProgress) Start() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return false
	}
	now := time.Now()
	p.running, p.phase, p.processed, p.total, p.currentFile = true, "scanning", 0, 0, ""
	p.startedAt, p.finishedAt, p.result, p.errorText = &now, nil, nil, nil
	p.cancel, p.cancelled = nil, false
	return true
}

func (p *IndexProgress) BindCancel(cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		p.cancel = cancel
	}
}

func (p *IndexProgress) Update(event rag.IndexProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running || p.cancelled {
		return
	}
	p.phase = event.Phase
	p.processed = event.Processed
	p.total = event.Total
	p.currentFile = event.CurrentFile
}

func (p *IndexProgress) Cancel() bool {
	p.mu.Lock()
	if !p.running || p.cancelled {
		p.mu.Unlock()
		return false
	}
	p.cancelled = true
	p.phase = "cancelling"
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (p *IndexProgress) Finish(result rag.IndexResult, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.running = false
	p.cancel = nil
	p.finishedAt = &now
	if err != nil {
		if errors.Is(err, context.Canceled) || p.cancelled {
			p.phase = "cancelled"
			p.errorText = nil
			return
		}
		value := "No se pudo completar la indexación."
		p.phase = "error"
		p.errorText = &value
		return
	}
	p.phase = "done"
	p.result = result
	p.total = result.Scanned
	p.processed = result.Scanned
}

func (p *IndexProgress) Snapshot() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	elapsed := 0.0
	if p.startedAt != nil {
		end := time.Now()
		if p.finishedAt != nil {
			end = *p.finishedAt
		}
		elapsed = end.Sub(*p.startedAt).Seconds()
	}
	percent := 0.0
	if p.total > 0 {
		percent = float64(p.processed) / float64(p.total) * 100
	}
	return map[string]any{"running": p.running, "phase": p.phase, "processed": p.processed, "total": p.total, "current_file": p.currentFile, "started_at": p.startedAt, "finished_at": p.finishedAt, "result": p.result, "error": p.errorText, "cancelled": p.cancelled, "cancellable": p.running && !p.cancelled, "elapsed_seconds": float64(int(elapsed*10)) / 10, "percent": float64(int(percent*10)) / 10}
}
