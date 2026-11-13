package explain

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/asaleem9/claimcheck/internal/snapshot"
)

// implicitPrefix lets any DeviceClass be requested as an extended resource.
const implicitPrefix = "deviceclass.resource.kubernetes.io/"

// extendedDemand is one extended resource a pod requests that a DeviceClass
// can back with DRA.
type extendedDemand struct {
	resource corev1.ResourceName
	class    *resourceapi.DeviceClass
	count    int64
}

// extendedDemands mirrors how kube-scheduler sizes the claim it creates for
// DRA-backed extended resources: one ExactCount request per container and
// resource, where short-lived init containers reuse devices, so the total
// per resource is max(sum over long-lived containers, largest short-lived).
func extendedDemands(snap *snapshot.Snapshot, pod *corev1.Pod) []extendedDemand {
	long := map[corev1.ResourceName]int64{}
	short := map[corev1.ResourceName]int64{}
	add := func(c corev1.Container, longLived bool) {
		for name, q := range c.Resources.Requests {
			if !extendedName(name) {
				continue
			}
			n, ok := q.AsInt64()
			if !ok || n <= 0 {
				continue
			}
			if longLived {
				long[name] += n
			} else {
				short[name] = max(short[name], n)
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		sidecar := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		add(c, sidecar)
	}
	for _, c := range pod.Spec.Containers {
		add(c, true)
	}
	var out []extendedDemand
	for _, name := range union(long, short) {
		class := classFor(snap, name)
		if class == nil {
			continue
		}
		out = append(out, extendedDemand{resource: name, class: class, count: max(long[name], short[name])})
	}
	return out
}

func union(a, b map[corev1.ResourceName]int64) []corev1.ResourceName {
	seen := map[corev1.ResourceName]bool{}
	var out []corev1.ResourceName
	for _, m := range []map[corev1.ResourceName]int64{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// extendedName reports whether a resource name can be an extended resource.
func extendedName(n corev1.ResourceName) bool {
	s := string(n)
	if strings.HasPrefix(s, implicitPrefix) {
		return true
	}
	domain, _, ok := strings.Cut(s, "/")
	return ok && domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io") && !strings.HasPrefix(s, "requests.")
}

// classFor finds the DeviceClass backing an extended resource: the implicit
// name first, otherwise the class with that extendedResourceName, the most
// recently created one if several claim it.
func classFor(snap *snapshot.Snapshot, n corev1.ResourceName) *resourceapi.DeviceClass {
	if name, ok := strings.CutPrefix(string(n), implicitPrefix); ok {
		c, err := snap.ClassLister().Get(name)
		if err != nil {
			return nil
		}
		return c
	}
	var best *resourceapi.DeviceClass
	for i := range snap.DeviceClasses {
		c := &snap.DeviceClasses[i]
		if c.Spec.ExtendedResourceName == nil || *c.Spec.ExtendedResourceName != string(n) {
			continue
		}
		if best == nil || c.CreationTimestamp.After(best.CreationTimestamp.Time) ||
			(c.CreationTimestamp.Equal(&best.CreationTimestamp) && c.Name > best.Name) {
			best = c
		}
	}
	return best
}

// extendedClaimFor builds the claim the scheduler would create for a node.
// Resources the node advertises in its allocatable come from a device
// plugin there, not DRA. It returns no claim when nothing goes through DRA.
func extendedClaimFor(pod *corev1.Pod, demands []extendedDemand, node *corev1.Node) (*resourceapi.ResourceClaim, []string) {
	var viaPlugin []string
	claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: pod.Namespace,
		Name:      pod.Name + "-extended-resources",
		UID:       types.UID("claimcheck-extended-" + string(pod.UID)),
	}}
	for _, d := range demands {
		if q, ok := node.Status.Allocatable[d.resource]; ok && !q.IsZero() {
			viaPlugin = append(viaPlugin, string(d.resource))
			continue
		}
		claim.Spec.Devices.Requests = append(claim.Spec.Devices.Requests, resourceapi.DeviceRequest{
			Name: string(d.resource),
			Exactly: &resourceapi.ExactDeviceRequest{
				DeviceClassName: d.class.Name,
				AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
				Count:           d.count,
			},
		})
	}
	if len(claim.Spec.Devices.Requests) == 0 {
		return nil, viaPlugin
	}
	return claim, viaPlugin
}

func describeDemands(ds []extendedDemand) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, fmt.Sprintf("%s: %d (DeviceClass %s)", d.resource, d.count, d.class.Name))
	}
	return strings.Join(parts, ", ")
}
