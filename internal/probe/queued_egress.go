package probe

import (
	"context"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

type egressProbeRequest struct {
	ctx  context.Context
	done chan egressProbeReply
}

type egressProbeReply struct {
	result *EgressProbeResult
	err    error
}

// ProbeEgressQueued shares the existing worker ceiling and start limiter.
// The subscription job submits one request at a time, instead of expanding
// the queue by the size of the subscription or running another worker pool.
func (m *ProbeManager) ProbeEgressQueued(ctx context.Context, hash node.Hash) (*EgressProbeResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.probeContext(), cancel)
	defer stop()
	defer cancel()
	request := &egressProbeRequest{ctx: ctx, done: make(chan egressProbeReply, 1)}
	task := probeTask{key: probeTaskKey{hash: hash, kind: probeTaskKindEgress}, request: request}
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if m.taskQueue.Enqueue(task, probePriorityHigh) {
			break
		}
		timer := time.NewTimer(probeStartInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-request.done:
		return reply.result, reply.err
	}
}
