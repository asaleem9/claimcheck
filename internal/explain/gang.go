package explain

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/utils/ptr"

	"github.com/asaleem9/claimcheck/internal/snapshot"
)

// Member is one pod of a PodGroup in the placement simulation.
type Member struct {
	Pod string `json:"pod"`
	// Placed means the member got its devices: it was already bound, or
	// the simulation placed it.
	Placed  bool     `json:"placed"`
	Bound   bool     `json:"bound,omitempty"`
	Node    string   `json:"node,omitempty"`
	Devices []string `json:"devices,omitempty"`
	// Report explains why a member couldn't be placed, given the devices
	// the members before it took.
	Report *Report `json:"report,omitempty"`
}

// GangReport explains whether a PodGroup's members can get their devices
// at the same time.
type GangReport struct {
	Target   Target   `json:"target"`
	Policy   string   `json:"policy"`
	MinCount int      `json:"minCount,omitempty"`
	Members  []Member `json:"members"`
	Placed   int      `json:"placed"`
	Status   Status   `json:"status"`
	Summary  string   `json:"summary"`
	Notes    []string `json:"notes,omitempty"`
}

// PodGroup simulates the scheduler's pod-group cycle for DRA: members are
// placed one at a time, in name order, on the first node where the real
// allocator fits their claims, and each placement's devices are reserved
// before the next member is tried. Claims the PodGroup itself owns are
// allocated with the first member and pin the rest.
func PodGroup(ctx context.Context, snap *snapshot.Snapshot, namespace, name string, opts Options) (*GangReport, error) {
	work := snap.DeepCopy()
	pg, err := work.PodGroup(namespace, name)
	if err != nil {
		return nil, err
	}
	rep := &GangReport{Target: Target{"podgroup", namespace, name}, Policy: "basic"}
	if g := pg.Spec.SchedulingPolicy.Gang; g != nil {
		rep.Policy, rep.MinCount = "gang", int(g.MinCount)
	}

	var groupClaims []string
	for i := range pg.Spec.ResourceClaims {
		n, _, err := resourceclaim.NameFromPodGroup(pg, &pg.Spec.ResourceClaims[i])
		if err != nil || n == nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("group claim %q has not been created yet", pg.Spec.ResourceClaims[i].Name))
			continue
		}
		groupClaims = append(groupClaims, *n)
	}

	var members []*corev1.Pod
	for i := range work.Pods {
		p := &work.Pods[i]
		if p.Namespace == namespace && p.Spec.SchedulingGroup != nil && ptr.Deref(p.Spec.SchedulingGroup.PodGroupName, "") == name {
			members = append(members, p)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	if len(members) == 0 {
		rep.Status = Blocked
		rep.Summary = "The PodGroup has no member pods yet."
		return rep, nil
	}

	sim := opts
	sim.Hypothetical = true
	sim.ExtraClaims = groupClaims
	firstFailure := Status("")
	for _, pod := range members {
		m := Member{Pod: pod.Name}
		if pod.Spec.NodeName != "" {
			m.Placed, m.Bound, m.Node = true, true, pod.Spec.NodeName
			rep.Members = append(rep.Members, m)
			rep.Placed++
			continue
		}
		r, err := Pod(ctx, work, namespace, pod.Name, sim)
		if err != nil {
			return nil, err
		}
		switch r.Status {
		case Fits:
			m.Placed, m.Node = true, r.FitNodes[0]
			m.Devices = reserve(work, pod, r, m.Node)
			pod.Spec.NodeName = m.Node
			rep.Placed++
		case Allocated:
			m.Placed = true
			rep.Placed++
		default:
			m.Report = r
			if firstFailure == "" {
				firstFailure = r.Status
			}
		}
		rep.Members = append(rep.Members, m)
	}

	need := rep.MinCount
	if rep.Policy == "basic" {
		need = len(members)
	}
	switch {
	case rep.Placed >= need && rep.Placed == len(members):
		rep.Status = Fits
		rep.Summary = fmt.Sprintf("All %d members can get their devices at the same time.", len(members))
	case rep.Placed >= need:
		rep.Status = Fits
		rep.Summary = fmt.Sprintf("%d of %d members can get their devices at the same time, enough for minCount %d.", rep.Placed, len(members), need)
	case firstFailure == Unexplained || firstFailure == AllocatorError:
		rep.Status = firstFailure
		rep.Summary = fmt.Sprintf("Only %d of %d members can get their devices; the first member that can't is %s.", rep.Placed, len(members), statusWord(firstFailure))
	default:
		rep.Status = Blocked
		if rep.Policy == "gang" {
			rep.Summary = fmt.Sprintf("Only %d of %d members can get their devices at the same time; the gang needs %d.", rep.Placed, len(members), need)
		} else {
			rep.Summary = fmt.Sprintf("Only %d of %d members can get their devices.", rep.Placed, len(members))
		}
	}
	rep.Notes = append(rep.Notes, "members are placed in name order on the first node that fits; the scheduler scores nodes, so a different placement might fit more members")
	return rep, nil
}

func statusWord(s Status) string {
	if s == AllocatorError {
		return "hitting an allocator error"
	}
	return "not fully explained"
}

// reserve records a placed member's allocation in the working snapshot so
// later members see those devices as taken.
func reserve(work *snapshot.Snapshot, pod *corev1.Pod, r *Report, node string) []string {
	var nr *NodeReport
	for i := range r.Nodes {
		if r.Nodes[i].Node == node {
			nr = &r.Nodes[i]
		}
	}
	if nr == nil {
		return nil
	}
	var devices []string
	for i, alloc := range nr.Verdict.Allocation {
		if i >= len(r.PendingClaims) {
			break
		}
		claim, err := work.Claim(pod.Namespace, r.PendingClaims[i])
		if err != nil {
			continue
		}
		claim.Status.Allocation = alloc.DeepCopy()
		claim.Status.ReservedFor = append(claim.Status.ReservedFor, resourceapi.ResourceClaimConsumerReference{
			Resource: "pods", Name: pod.Name, UID: pod.UID,
		})
		for _, res := range alloc.Devices.Results {
			devices = append(devices, res.Device)
		}
	}
	return devices
}
