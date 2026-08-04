package probe

import (
	"strings"
	"sync"

	"github.com/Resinat/Resin/internal/node"
)

const maxProbeFailureSampleRunes = 256

type probeFailureKind uint8

const (
	probeFailureEgressFetch probeFailureKind = iota
	probeFailureEgressParse
	probeFailureLatency
	probeFailureKindCount
)

type probeFailureBucket struct {
	count       uint64
	sampleNode  string
	sampleError string
}

type probeFailureSnapshot struct {
	buckets [probeFailureKindCount]probeFailureBucket
}

// Empty reports whether the snapshot contains any failures.
func (s probeFailureSnapshot) Empty() bool {
	for _, bucket := range s.buckets {
		if bucket.count > 0 {
			return false
		}
	}
	return true
}

type probeFailureReporter struct {
	mu      sync.Mutex
	buckets [probeFailureKindCount]probeFailureBucket
}

// Record adds one failure and keeps only the first bounded sample in the window.
func (r *probeFailureReporter) Record(kind probeFailureKind, hash node.Hash, err error) {
	if r == nil || kind >= probeFailureKindCount || err == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := &r.buckets[kind]
	bucket.count++
	if bucket.sampleError != "" {
		return
	}
	bucket.sampleNode = hash.Hex()
	bucket.sampleError = boundedProbeFailureSample(err.Error())
}

// Drain returns the current counters and starts a new empty reporting window.
func (r *probeFailureReporter) Drain() probeFailureSnapshot {
	if r == nil {
		return probeFailureSnapshot{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := probeFailureSnapshot{buckets: r.buckets}
	r.buckets = [probeFailureKindCount]probeFailureBucket{}
	return snapshot
}

func boundedProbeFailureSample(raw string) string {
	sample := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	runes := []rune(sample)
	if len(runes) <= maxProbeFailureSampleRunes {
		return sample
	}
	return string(runes[:maxProbeFailureSampleRunes])
}
