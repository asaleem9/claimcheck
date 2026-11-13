// Package verdict answers "would these claims fit on this node?" by running
// the exact allocator kube-scheduler uses (k8s.io/dynamic-resource-allocation/structured).
// It never explains anything; that's the job of package diagnose. Keeping the
// two separate is what lets claimcheck detect when its explanation is wrong.
package verdict

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"
)

// Result is the allocator's answer for one node.
type Result struct {
	Fits bool `json:"fits"`
	// Allocation holds one result per claim, in input order, when Fits is true.
	Allocation []resourceapi.AllocationResult `json:"allocation,omitempty"`
	// Err is set when the allocator failed outright (e.g. an invalid CEL
	// selector). In that case the scheduler also reports an error, not just
	// "cannot allocate".
	Err string `json:"error,omitempty"`
	// NodeReason is set when allocation failed for a reason the allocator
	// itself reports for this node only.
	NodeReason string `json:"nodeReason,omitempty"`
}

// Input is everything the allocator needs.
type Input struct {
	Features  structured.Features
	Allocated structured.AllocatedState
	Classes   structured.DeviceClassLister
	Slices    []*resourceapi.ResourceSlice
	CEL       *cel.Cache
}

// Evaluator runs one allocator instance against many nodes, which is how the
// scheduler uses it too (built once in PreFilter, called per node in Filter).
type Evaluator struct {
	alloc structured.Allocator
}

func New(ctx context.Context, in Input) (*Evaluator, error) {
	a, err := structured.NewAllocator(ctx, in.Features, in.Allocated, in.Classes, in.Slices, in.CEL)
	if err != nil {
		return nil, fmt.Errorf("create allocator: %w", err)
	}
	return &Evaluator{alloc: a}, nil
}

// Evaluate asks the allocator to place all claims on node at once. Claims of
// one pod must be allocated together, so they are never evaluated one by one.
func (e *Evaluator) Evaluate(ctx context.Context, node *corev1.Node, claims []*resourceapi.ResourceClaim) Result {
	results, err := e.alloc.Allocate(ctx, node, claims)
	if errors.Is(err, structured.ErrFailedAllocationOnNode) {
		// Only this node is affected (e.g. invalid pools); the scheduler
		// shows the message as the node's unschedulable reason.
		return Result{NodeReason: err.Error()}
	}
	if err != nil {
		return Result{Err: err.Error()}
	}
	if len(results) != len(claims) {
		return Result{}
	}
	return Result{Fits: true, Allocation: results}
}
