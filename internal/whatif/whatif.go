// Package whatif applies hypothetical changes to a snapshot (free a claim,
// free a pod's claims, remove a taint rule, add a claim) so a pod or claim
// can be explained against the changed cluster, and searches for the single
// change that would make it fit.
package whatif

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"sigs.k8s.io/yaml"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/snapshot"
)

// Kind of change.
type Kind string

const (
	FreeClaim       Kind = "free-claim"
	FreePod         Kind = "free-pod"
	RemoveTaintRule Kind = "remove-taint-rule"
	AddClaim        Kind = "add-claim"
)

// Change is one hypothetical edit to the cluster.
type Change struct {
	Kind      Kind   `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// claim is set for AddClaim.
	claim *resourceapi.ResourceClaim
}

func (c Change) String() string {
	target := c.Name
	if c.Namespace != "" {
		target = c.Namespace + "/" + c.Name
	}
	switch c.Kind {
	case FreeClaim:
		return "free ResourceClaim " + target
	case FreePod:
		return "free the claims of pod " + target
	case RemoveTaintRule:
		return "remove DeviceTaintRule " + target
	case AddClaim:
		return "add ResourceClaim " + target
	}
	return string(c.Kind) + " " + target
}

// Apply returns a copy of snap with the changes made.
func Apply(snap *snapshot.Snapshot, changes []Change) (*snapshot.Snapshot, error) {
	out := snap.DeepCopy()
	for _, c := range changes {
		switch c.Kind {
		case FreeClaim:
			cl, err := out.Claim(c.Namespace, c.Name)
			if err != nil {
				return nil, err
			}
			if cl.Status.Allocation == nil {
				return nil, fmt.Errorf("ResourceClaim %s/%s is not allocated, nothing to free", c.Namespace, c.Name)
			}
			cl.Status.Allocation, cl.Status.ReservedFor = nil, nil
		case FreePod:
			pod, err := out.Pod(c.Namespace, c.Name)
			if err != nil {
				return nil, err
			}
			freed := 0
			for i := range pod.Spec.ResourceClaims {
				name, _, err := resourceclaim.Name(pod, &pod.Spec.ResourceClaims[i])
				if err != nil || name == nil {
					continue
				}
				if cl, err := out.Claim(c.Namespace, *name); err == nil && cl.Status.Allocation != nil {
					cl.Status.Allocation, cl.Status.ReservedFor = nil, nil
					freed++
				}
			}
			if freed == 0 {
				return nil, fmt.Errorf("pod %s/%s holds no allocated ResourceClaims", c.Namespace, c.Name)
			}
		case RemoveTaintRule:
			found := false
			kept := out.DeviceTaintRules[:0]
			for _, r := range out.DeviceTaintRules {
				if r.Name == c.Name {
					found = true
					continue
				}
				kept = append(kept, r)
			}
			if !found {
				return nil, fmt.Errorf("DeviceTaintRule %s not found", c.Name)
			}
			out.DeviceTaintRules = kept
		case AddClaim:
			if _, err := out.Claim(c.Namespace, c.Name); err == nil {
				return nil, fmt.Errorf("ResourceClaim %s/%s already exists", c.Namespace, c.Name)
			}
			out.ResourceClaims = append(out.ResourceClaims, *c.claim.DeepCopy())
		default:
			return nil, fmt.Errorf("unknown change %q", c.Kind)
		}
	}
	return out, nil
}

// ReadClaims reads ResourceClaims or ResourceClaimTemplates from a YAML file
// (multiple documents allowed) and turns them into AddClaim changes.
// Templates become claims named after the template.
func ReadClaims(path, defaultNamespace string) ([]Change, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := utilyaml.NewYAMLReader(bufio.NewReader(f))
	var out []Change
	for {
		doc, err := dec.Read()
		if err != nil {
			break
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var claim resourceapi.ResourceClaim
		switch meta.Kind {
		case "ResourceClaim":
			if err := yaml.Unmarshal(doc, &claim); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case "ResourceClaimTemplate":
			var t resourceapi.ResourceClaimTemplate
			if err := yaml.Unmarshal(doc, &t); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			claim.ObjectMeta = t.ObjectMeta
			claim.Spec = t.Spec.Spec
		default:
			continue
		}
		if claim.Namespace == "" {
			claim.Namespace = defaultNamespace
		}
		claim.UID = types.UID("whatif-" + claim.Namespace + "-" + claim.Name)
		applyDefaults(&claim)
		claim.Status = resourceapi.ResourceClaimStatus{}
		out = append(out, Change{Kind: AddClaim, Namespace: claim.Namespace, Name: claim.Name, claim: &claim})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no ResourceClaim or ResourceClaimTemplate found", path)
	}
	return out, nil
}

// Suggestion is a single change that makes the target fit.
type Suggestion struct {
	Change Change   `json:"change"`
	Nodes  []string `json:"nodes"`
	// Holds describes what the change releases, e.g. "6 devices on node-a".
	Holds string `json:"holds,omitempty"`
}

// Target names what to explain.
type Target struct {
	Kind, Namespace, Name string
}

// Explain runs explain for the target against snap.
func Explain(ctx context.Context, snap *snapshot.Snapshot, t Target, opts explain.Options) (*explain.Report, error) {
	if t.Kind == "pod" {
		return explain.Pod(ctx, snap, t.Namespace, t.Name, opts)
	}
	return explain.ResourceClaim(ctx, snap, t.Namespace, t.Name, opts)
}

// Suggest tries every single change that could plausibly help (freeing each
// allocated claim, removing each taint rule) and returns those after which
// the target fits, smallest release first.
func Suggest(ctx context.Context, snap *snapshot.Snapshot, t Target, opts explain.Options, limit int) ([]Suggestion, int, error) {
	var candidates []Change
	type holding struct {
		n     int
		nodes map[string]bool
	}
	held := map[string]holding{}
	for i := range snap.ResourceClaims {
		c := &snap.ResourceClaims[i]
		if c.Status.Allocation == nil {
			continue
		}
		key := c.Namespace + "/" + c.Name
		h := holding{n: len(c.Status.Allocation.Devices.Results), nodes: map[string]bool{}}
		for _, r := range c.Status.Allocation.Devices.Results {
			h.nodes[r.Pool] = true
		}
		held[key] = h
		candidates = append(candidates, Change{Kind: FreeClaim, Namespace: c.Namespace, Name: c.Name})
	}
	for _, r := range snap.DeviceTaintRules {
		candidates = append(candidates, Change{Kind: RemoveTaintRule, Name: r.Name})
	}
	tried := 0
	var out []Suggestion
	for _, c := range candidates {
		if tried >= limit {
			break
		}
		tried++
		changed, err := Apply(snap, []Change{c})
		if err != nil {
			continue
		}
		rep, err := Explain(ctx, changed, t, opts)
		if err != nil {
			return nil, tried, err
		}
		if rep.Status != explain.Fits {
			continue
		}
		s := Suggestion{Change: c, Nodes: rep.FitNodes}
		if c.Kind == FreeClaim {
			h := held[c.Namespace+"/"+c.Name]
			pools := make([]string, 0, len(h.nodes))
			for p := range h.nodes {
				pools = append(pools, p)
			}
			sort.Strings(pools)
			s.Holds = fmt.Sprintf("%d device%s in %s", h.n, map[bool]string{true: "", false: "s"}[h.n == 1], strings.Join(pools, ", "))
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return releaseSize(out[i]) < releaseSize(out[j]) })
	return out, tried, nil
}

func releaseSize(s Suggestion) int {
	if s.Change.Kind != FreeClaim {
		return 1 << 20 // taint rules after claims: they usually exist for a reason
	}
	var n int
	_, _ = fmt.Sscanf(s.Holds, "%d", &n)
	return n
}

// applyDefaults sets the defaults the API server would, since claims read
// from a file haven't been through it: allocationMode ExactCount, count 1.
func applyDefaults(c *resourceapi.ResourceClaim) {
	def := func(mode *resourceapi.DeviceAllocationMode, count *int64) {
		if *mode == "" {
			*mode = resourceapi.DeviceAllocationModeExactCount
		}
		if *mode == resourceapi.DeviceAllocationModeExactCount && *count == 0 {
			*count = 1
		}
	}
	for i := range c.Spec.Devices.Requests {
		r := &c.Spec.Devices.Requests[i]
		if r.Exactly != nil {
			def(&r.Exactly.AllocationMode, &r.Exactly.Count)
		}
		for j := range r.FirstAvailable {
			def(&r.FirstAvailable[j].AllocationMode, &r.FirstAvailable[j].Count)
		}
	}
}
