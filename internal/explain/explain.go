// Package explain ties a pod or ResourceClaim to a per-node verdict from the
// real allocator plus a per-device diagnosis, and checks that the two agree.
package explain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/dynamic-resource-allocation/structured"

	"github.com/asaleem9/claimcheck/internal/diagnose"
	"github.com/asaleem9/claimcheck/internal/features"
	"github.com/asaleem9/claimcheck/internal/snapshot"
	"github.com/asaleem9/claimcheck/internal/verdict"
)

// Status is the overall outcome.
type Status string

const (
	// Fits: the allocator can place the claims on at least one eligible node.
	Fits Status = "Fits"
	// Blocked: no eligible node works, and the diagnosis agrees with the allocator everywhere.
	Blocked Status = "Blocked"
	// Unexplained: the diagnosis disagrees with the allocator on some node.
	// claimcheck shows both answers instead of guessing.
	Unexplained Status = "Unexplained"
	// Allocated: every claim already has an allocation; DRA isn't the blocker.
	Allocated Status = "Allocated"
	// AllocatorError: the allocator itself failed, as the scheduler would.
	AllocatorError Status = "AllocatorError"
)

type Target struct {
	Kind      string `json:"kind"` // "pod" or "resourceclaim"
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (t Target) String() string { return t.Kind + " " + t.Namespace + "/" + t.Name }

type Claim struct {
	Name      string `json:"name"`
	PodClaim  string `json:"podClaim,omitempty"`
	Allocated bool   `json:"allocated"`
}

type NodeReport struct {
	Node      string         `json:"node"`
	Verdict   verdict.Result `json:"verdict"`
	Diagnosis diagnose.Node  `json:"diagnosis"`
	// Agree is false when the diagnosis contradicts the allocator.
	Agree bool `json:"agree"`
	// Excluded explains why the pod can't use this node regardless of DRA.
	Excluded string `json:"excluded,omitempty"`
}

type Report struct {
	Target   Target       `json:"target"`
	Phase    string       `json:"phase,omitempty"`
	Status   Status       `json:"status"`
	Summary  string       `json:"summary"`
	Claims   []Claim      `json:"claims"`
	Nodes    []NodeReport `json:"nodes,omitempty"`
	Hints    []string     `json:"hints,omitempty"`
	Notes    []string     `json:"notes,omitempty"`
	FitNodes []string     `json:"fitNodes,omitempty"`
	// PendingClaims lists the unallocated claims in the order the allocator
	// received them; each node's verdict allocation follows this order.
	PendingClaims []string `json:"pendingClaims,omitempty"`
}

type Options struct {
	Features structured.Features
	// Hypothetical marks a what-if run, where the pod's real status says
	// nothing about the changed cluster.
	Hypothetical bool
	// ExtraClaims names claims (in the pod's namespace) to consider with
	// the pod's own, such as claims shared by its PodGroup.
	ExtraClaims []string
}

// Pod explains why a pod's ResourceClaims can or can't be allocated.
func Pod(ctx context.Context, snap *snapshot.Snapshot, namespace, name string, opts Options) (*Report, error) {
	pod, err := snap.Pod(namespace, name)
	if err != nil {
		return nil, err
	}
	rep := &Report{Target: Target{"pod", namespace, name}, Phase: string(pod.Status.Phase)}
	var pending []*resourceapi.ResourceClaim
	var allocated []*resourceapi.ResourceClaim
	for i := range pod.Spec.ResourceClaims {
		pc := &pod.Spec.ResourceClaims[i]
		claimName, mustCheckOwner, err := resourceclaim.Name(pod, pc)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("pod claim %q: %v (the resourceclaim controller may not have created it yet)", pc.Name, err))
			continue
		}
		if claimName == nil {
			continue
		}
		claim, err := snap.Claim(namespace, *claimName)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("pod claim %q: ResourceClaim %s not found", pc.Name, *claimName))
			continue
		}
		if mustCheckOwner {
			if err := resourceclaim.IsForPod(pod, claim, true); err != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("pod claim %q: %v", pc.Name, err))
				continue
			}
		}
		c := Claim{Name: claim.Name, PodClaim: pc.Name, Allocated: claim.Status.Allocation != nil}
		rep.Claims = append(rep.Claims, c)
		if c.Allocated {
			allocated = append(allocated, claim)
		} else {
			pending = append(pending, claim)
		}
	}
	// Claims shared by the pod's group are allocated with the first member
	// and pin the others to the nodes they were allocated for.
	for _, name := range opts.ExtraClaims {
		claim, err := snap.Claim(namespace, name)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("group claim %s not found", name))
			continue
		}
		if hasClaim(rep.Claims, claim.Name) {
			continue
		}
		c := Claim{Name: claim.Name, PodClaim: "(pod group)", Allocated: claim.Status.Allocation != nil}
		rep.Claims = append(rep.Claims, c)
		if c.Allocated {
			allocated = append(allocated, claim)
		} else {
			pending = append(pending, claim)
		}
	}
	if pod.Status.ExtendedResourceClaimStatus != nil {
		rep.Notes = append(rep.Notes, "pod uses extended resources backed by DRA; those requests are not analyzed yet")
	}
	return run(ctx, snap, rep, pod, pending, allocated, opts)
}

func hasClaim(cs []Claim, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}

// ResourceClaim explains a single ResourceClaim as if a pod with only that claim
// were being scheduled.
func ResourceClaim(ctx context.Context, snap *snapshot.Snapshot, namespace, name string, opts Options) (*Report, error) {
	claim, err := snap.Claim(namespace, name)
	if err != nil {
		return nil, err
	}
	rep := &Report{Target: Target{"resourceclaim", namespace, name}}
	c := Claim{Name: claim.Name, Allocated: claim.Status.Allocation != nil}
	rep.Claims = append(rep.Claims, c)
	if c.Allocated {
		return run(ctx, snap, rep, nil, nil, []*resourceapi.ResourceClaim{claim}, opts)
	}
	return run(ctx, snap, rep, nil, []*resourceapi.ResourceClaim{claim}, nil, opts)
}

func run(ctx context.Context, snap *snapshot.Snapshot, rep *Report, pod *corev1.Pod, pending, allocated []*resourceapi.ResourceClaim, opts Options) (*Report, error) {
	if len(pending) == 0 {
		rep.Status = Allocated
		switch {
		case len(rep.Claims) == 0:
			rep.Summary = "No ResourceClaims to allocate; DRA is not what's blocking this."
		default:
			rep.Summary = "All ResourceClaims are already allocated; DRA is not what's blocking this."
		}
		for _, c := range allocated {
			if ns := c.Status.Allocation.NodeSelector; ns != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s is allocated and pins the pod to nodes matching %s", c.Name, formatNodeSelector(ns)))
			}
		}
		return rep, nil
	}

	skip := sets.New[types.UID]()
	for _, c := range pending {
		skip.Insert(c.UID)
		rep.PendingClaims = append(rep.PendingClaims, c.Name)
	}
	dg := NewDiagnoser(snap, opts.Features, skip)
	ev, err := verdict.New(ctx, verdict.Input{
		Features: opts.Features, Allocated: dg.Allocated, Classes: dg.Classes, Slices: dg.Slices, CEL: dg.CEL,
	})
	if err != nil {
		return nil, err
	}

	nodes := make([]*corev1.Node, 0, len(snap.Nodes))
	for i := range snap.Nodes {
		nodes = append(nodes, &snap.Nodes[i])
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	disagree, eligible := 0, 0
	for _, node := range nodes {
		nr := NodeReport{Node: node.Name, Excluded: excluded(opts.Features, pod, allocated, node)}
		nr.Verdict = ev.Evaluate(ctx, node, pending)
		nr.Diagnosis, err = dg.Diagnose(ctx, node, pending)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", node.Name, err)
		}
		switch {
		case nr.Verdict.Err != "":
			nr.Agree = hasFatal(nr.Diagnosis) || IsConstraintError(nr.Verdict.Err)
		case nr.Verdict.NodeReason != "":
			nr.Agree = true
		default:
			nr.Agree = nr.Verdict.Fits == nr.Diagnosis.Fits
			// A "no" only counts as explained if the diagnosis names a reason.
			if !nr.Verdict.Fits && !hasReason(nr.Diagnosis) {
				nr.Agree = false
			}
		}
		if nr.Excluded == "" {
			eligible++
			if !nr.Agree {
				disagree++
			}
			if nr.Verdict.Fits {
				rep.FitNodes = append(rep.FitNodes, node.Name)
			}
		}
		rep.Nodes = append(rep.Nodes, nr)
	}

	switch {
	case anyAllocatorError(rep.Nodes):
		rep.Status = AllocatorError
		rep.Summary = "The allocator fails with an error, so the scheduler can't place this on any node."
	case len(rep.FitNodes) > 0:
		rep.Status = Fits
		rep.Summary = fmt.Sprintf("The claims can be allocated on %d of %s.", len(rep.FitNodes), plural(eligible, "eligible node"))
		if pod != nil && pod.Status.Phase == corev1.PodPending && !opts.Hypothetical {
			rep.Notes = append(rep.Notes, "the pod is Pending even though DRA would fit; check other scheduling constraints or whether the scheduler has retried since the cluster changed")
		}
	case eligible == 0:
		rep.Status = Blocked
		rep.Summary = "No node is eligible for this pod, independent of DRA."
	case disagree > 0:
		rep.Status = Unexplained
		rep.Summary = fmt.Sprintf("No eligible node can allocate the claims, but claimcheck's explanation disagrees with the allocator on %s.", plural(disagree, "node"))
	default:
		rep.Status = Blocked
		rep.Summary = fmt.Sprintf("No eligible node can allocate the claims (%s checked).", plural(eligible, "node"))
	}
	rep.Hints = hints(rep)
	return rep, nil
}

// NewDiagnoser builds the cluster state shared by every node: allocations
// (ignoring the claims in skip), slices with taint rules applied, and a
// CEL cache.
func NewDiagnoser(snap *snapshot.Snapshot, f structured.Features, skip sets.Set[types.UID]) *diagnose.Diagnoser {
	allocState, owners := snap.AllocatedState(skip)
	slices, taints := snap.EffectiveSlices()
	return &diagnose.Diagnoser{
		Features: f, Classes: snap.ClassLister(), Slices: slices,
		Allocated: allocState, Owners: owners, Taints: taints,
		CEL: cel.NewCache(1000, features.CEL(f)),
	}
}

// excluded reports why a pod can't use a node for reasons outside DRA that
// are cheap to check: an explicit nodeName, nodeSelector, or an already
// allocated claim pinned elsewhere. Other scheduler filters are out of scope.
func excluded(f structured.Features, pod *corev1.Pod, allocated []*resourceapi.ResourceClaim, node *corev1.Node) string {
	if pod != nil {
		if pod.Spec.NodeName != "" && pod.Spec.NodeName != node.Name {
			return "pod.spec.nodeName is " + pod.Spec.NodeName
		}
		for k, v := range pod.Spec.NodeSelector {
			if node.Labels[k] != v {
				return fmt.Sprintf("nodeSelector %s=%s does not match", k, v)
			}
		}
	}
	for _, c := range allocated {
		if ns := c.Status.Allocation.NodeSelector; ns != nil {
			ok, err := structured.NodeMatches(f, node, "", false, ns)
			if err == nil && !ok {
				return "already-allocated claim " + c.Name + " is pinned to other nodes"
			}
		}
	}
	return ""
}

// IsConstraintError recognizes the allocator's hard error for a device of an
// allocationMode: All request that breaks a claim constraint. The allocator
// doesn't backtrack in that case, so the error itself is the explanation.
func IsConstraintError(err string) bool {
	return strings.Contains(err, "because a claim constraint would not be satisfied")
}

// hasReason reports whether the diagnosis found a concrete reason the
// claims don't fit, as opposed to giving up on an unmodeled feature.
func hasReason(n diagnose.Node) bool {
	if n.Contention != "" || n.ConstraintFailure != "" || n.CounterConflict != "" {
		return true
	}
	for _, r := range n.Requests {
		if !r.Satisfied {
			return true
		}
	}
	return false
}

// hasFatal reports whether the diagnosis found something the allocator
// treats as an error rather than as "doesn't fit".
func hasFatal(n diagnose.Node) bool {
	for _, r := range n.Requests {
		for _, rr := range append([]diagnose.Request{r}, r.Alternatives...) {
			if rr.Fatal {
				return true
			}
			for _, d := range rr.Devices {
				if d.Reason == diagnose.SelectorError {
					return true
				}
			}
		}
	}
	return false
}

func anyAllocatorError(nodes []NodeReport) bool {
	for _, n := range nodes {
		if n.Verdict.Err != "" && n.Excluded == "" {
			return true
		}
	}
	return false
}

func formatNodeSelector(ns *corev1.NodeSelector) string {
	var out string
	for i, t := range ns.NodeSelectorTerms {
		if i > 0 {
			out += " OR "
		}
		for j, e := range t.MatchFields {
			if j > 0 {
				out += ","
			}
			out += fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values)
		}
		for j, e := range t.MatchExpressions {
			if j > 0 || len(t.MatchFields) > 0 {
				out += ","
			}
			out += fmt.Sprintf("%s %s %v", e.Key, e.Operator, e.Values)
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
