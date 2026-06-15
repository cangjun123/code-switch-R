package services

import (
	"sort"
	"sync"
	"time"
)

const (
	requestLogStatusCompleted  = "completed"
	requestLogStatusProcessing = "processing"
)

var defaultActiveRequestTracker = newActiveRequestTracker()

type activeRequestTracker struct {
	mu       sync.RWMutex
	nextID   int64
	requests map[int64]activeRequestSnapshot
}

type activeRequestSnapshot struct {
	startedAt time.Time
	log       ReqeustLog
}

func newActiveRequestTracker() *activeRequestTracker {
	return &activeRequestTracker{
		requests: make(map[int64]activeRequestSnapshot),
	}
}

func (t *activeRequestTracker) Start(logEntry *ReqeustLog, startedAt time.Time) int64 {
	if t == nil || logEntry == nil {
		return 0
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.nextID++
	id := t.nextID
	snapshot := snapshotActiveRequest(id, logEntry, startedAt)
	t.requests[id] = snapshot
	return id
}

func (t *activeRequestTracker) Update(id int64, logEntry *ReqeustLog) {
	if t == nil || id == 0 || logEntry == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.requests[id]
	if !ok {
		return
	}
	t.requests[id] = snapshotActiveRequest(id, logEntry, existing.startedAt)
}

func (t *activeRequestTracker) Finish(id int64) {
	if t == nil || id == 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.requests, id)
}

func (t *activeRequestTracker) List(platform, provider string, loc *time.Location) []ReqeustLog {
	if t == nil {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}

	now := time.Now()
	t.mu.RLock()
	snapshots := make([]activeRequestSnapshot, 0, len(t.requests))
	for _, snapshot := range t.requests {
		if platform != "" && snapshot.log.Platform != platform {
			continue
		}
		if provider != "" && snapshot.log.Provider != provider {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	t.mu.RUnlock()

	sort.SliceStable(snapshots, func(i, j int) bool {
		return snapshots[i].startedAt.After(snapshots[j].startedAt)
	})

	logs := make([]ReqeustLog, 0, len(snapshots))
	for _, snapshot := range snapshots {
		logEntry := snapshot.log
		logEntry.DurationSec = now.Sub(snapshot.startedAt).Seconds()
		if logEntry.DurationSec < 0 {
			logEntry.DurationSec = 0
		}
		logEntry.CreatedAt = snapshot.startedAt.In(loc).Format(timeLayout)
		logs = append(logs, logEntry)
	}
	return logs
}

func snapshotActiveRequest(id int64, logEntry *ReqeustLog, startedAt time.Time) activeRequestSnapshot {
	snapshot := *logEntry
	snapshot.ID = -id
	snapshot.HttpCode = 0
	snapshot.DurationSec = 0
	snapshot.CreatedAt = startedAt.Format(timeLayout)
	snapshot.Status = requestLogStatusProcessing
	snapshot.InputCost = 0
	snapshot.OutputCost = 0
	snapshot.ReasoningCost = 0
	snapshot.CacheCreateCost = 0
	snapshot.CacheReadCost = 0
	snapshot.Ephemeral5mCost = 0
	snapshot.Ephemeral1hCost = 0
	snapshot.TotalCost = 0
	snapshot.HasPricing = false
	snapshot.SkipLog = false
	return activeRequestSnapshot{
		startedAt: startedAt,
		log:       snapshot,
	}
}
