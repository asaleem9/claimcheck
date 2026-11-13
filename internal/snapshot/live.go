package snapshot

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// FromCluster reads every object DRA allocation depends on. It only issues
// list calls, so it needs read access to nodes, pods and resource.k8s.io.
func FromCluster(ctx context.Context, cs kubernetes.Interface, contextName string) (*Snapshot, error) {
	s := &Snapshot{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{CapturedAt: metav1.Now(), Context: contextName},
	}
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		s.Metadata.ServerVersion = v.GitVersion
	}

	all := metav1.ListOptions{}
	nodes, err := cs.CoreV1().Nodes().List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		strip(&n.ObjectMeta)
		n.Status.Images = nil
		s.Nodes = append(s.Nodes, n)
	}

	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		if !usesDRA(&p) {
			continue
		}
		strip(&p.ObjectMeta)
		s.Pods = append(s.Pods, p)
	}

	r := cs.ResourceV1()
	classes, err := r.DeviceClasses().List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list deviceclasses (is DRA enabled and resource.k8s.io/v1 served?): %w", err)
	}
	for _, c := range classes.Items {
		strip(&c.ObjectMeta)
		s.DeviceClasses = append(s.DeviceClasses, c)
	}

	slices, err := r.ResourceSlices().List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list resourceslices: %w", err)
	}
	for _, sl := range slices.Items {
		strip(&sl.ObjectMeta)
		s.ResourceSlices = append(s.ResourceSlices, sl)
	}

	claims, err := r.ResourceClaims(metav1.NamespaceAll).List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list resourceclaims: %w", err)
	}
	for _, c := range claims.Items {
		strip(&c.ObjectMeta)
		s.ResourceClaims = append(s.ResourceClaims, c)
	}

	rules, err := r.DeviceTaintRules().List(ctx, all)
	switch {
	case err == nil:
		for _, tr := range rules.Items {
			strip(&tr.ObjectMeta)
			s.DeviceTaintRules = append(s.DeviceTaintRules, tr)
		}
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
		// Older clusters don't serve DeviceTaintRules and some users can't
		// read them. Taints set directly in slices are still analyzed.
	default:
		return nil, fmt.Errorf("list devicetaintrules: %w", err)
	}

	groups, err := cs.SchedulingV1beta1().PodGroups(metav1.NamespaceAll).List(ctx, all)
	switch {
	case err == nil:
		for _, pg := range groups.Items {
			strip(&pg.ObjectMeta)
			s.PodGroups = append(s.PodGroups, pg)
		}
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
		// The Workload/PodGroup API is beta in v1.37 and not served by
		// default; without it there are no gangs to explain.
	default:
		return nil, fmt.Errorf("list podgroups: %w", err)
	}

	s.sort()
	return s, nil
}

func usesDRA(p *corev1.Pod) bool {
	return len(p.Spec.ResourceClaims) > 0 || p.Status.ExtendedResourceClaimStatus != nil || p.Spec.SchedulingGroup != nil
}
