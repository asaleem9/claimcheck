// Package snapshot captures the cluster state that DRA allocation depends on,
// either live from an API server or from a file written earlier, so that
// allocation can be replayed and explained offline.
package snapshot

import (
	"fmt"
	"os"
	"sort"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	schedulingapi "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "claimcheck.dev/v1alpha1"
	Kind       = "Snapshot"
)

// Snapshot is the complete input to an allocation replay.
type Snapshot struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`

	Nodes            []corev1.Node                 `json:"nodes,omitempty"`
	Pods             []corev1.Pod                  `json:"pods,omitempty"`
	DeviceClasses    []resourceapi.DeviceClass     `json:"deviceClasses,omitempty"`
	ResourceSlices   []resourceapi.ResourceSlice   `json:"resourceSlices,omitempty"`
	ResourceClaims   []resourceapi.ResourceClaim   `json:"resourceClaims,omitempty"`
	DeviceTaintRules []resourceapi.DeviceTaintRule `json:"deviceTaintRules,omitempty"`
	PodGroups        []schedulingapi.PodGroup      `json:"podGroups,omitempty"`
}

type Metadata struct {
	CapturedAt    metav1.Time `json:"capturedAt,omitempty"`
	Context       string      `json:"context,omitempty"`
	ServerVersion string      `json:"serverVersion,omitempty"`
}

// ReadFile loads a snapshot written by WriteFile or by `kubectl claimcheck snapshot`.
func ReadFile(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, fmt.Errorf("parse snapshot %s: %w", path, err)
	}
	if s.Kind != Kind || s.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: expected %s/%s, got apiVersion=%q kind=%q", path, APIVersion, Kind, s.APIVersion, s.Kind)
	}
	return &s, nil
}

// Marshal renders the snapshot as YAML with a stable object order.
func (s *Snapshot) Marshal() ([]byte, error) {
	s.APIVersion, s.Kind = APIVersion, Kind
	s.sort()
	return yaml.Marshal(s)
}

func (s *Snapshot) sort() {
	sort.Slice(s.Nodes, func(i, j int) bool { return s.Nodes[i].Name < s.Nodes[j].Name })
	sort.Slice(s.Pods, func(i, j int) bool { return key(&s.Pods[i].ObjectMeta) < key(&s.Pods[j].ObjectMeta) })
	sort.Slice(s.DeviceClasses, func(i, j int) bool { return s.DeviceClasses[i].Name < s.DeviceClasses[j].Name })
	sort.Slice(s.ResourceSlices, func(i, j int) bool { return s.ResourceSlices[i].Name < s.ResourceSlices[j].Name })
	sort.Slice(s.ResourceClaims, func(i, j int) bool {
		return key(&s.ResourceClaims[i].ObjectMeta) < key(&s.ResourceClaims[j].ObjectMeta)
	})
	sort.Slice(s.DeviceTaintRules, func(i, j int) bool { return s.DeviceTaintRules[i].Name < s.DeviceTaintRules[j].Name })
	sort.Slice(s.PodGroups, func(i, j int) bool { return key(&s.PodGroups[i].ObjectMeta) < key(&s.PodGroups[j].ObjectMeta) })
}

func key(m *metav1.ObjectMeta) string { return m.Namespace + "/" + m.Name }

// Pod returns the named pod or a NotFound error.
func (s *Snapshot) Pod(namespace, name string) (*corev1.Pod, error) {
	for i := range s.Pods {
		if s.Pods[i].Namespace == namespace && s.Pods[i].Name == name {
			return &s.Pods[i], nil
		}
	}
	return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
}

// Claim returns the named ResourceClaim or a NotFound error.
func (s *Snapshot) Claim(namespace, name string) (*resourceapi.ResourceClaim, error) {
	for i := range s.ResourceClaims {
		if s.ResourceClaims[i].Namespace == namespace && s.ResourceClaims[i].Name == name {
			return &s.ResourceClaims[i], nil
		}
	}
	return nil, apierrors.NewNotFound(resourceapi.Resource("resourceclaims"), name)
}

// PodGroup returns the named PodGroup or a NotFound error.
func (s *Snapshot) PodGroup(namespace, name string) (*schedulingapi.PodGroup, error) {
	for i := range s.PodGroups {
		if s.PodGroups[i].Namespace == namespace && s.PodGroups[i].Name == name {
			return &s.PodGroups[i], nil
		}
	}
	return nil, apierrors.NewNotFound(schedulingapi.Resource("podgroups"), name)
}

// Node returns the named node, or nil.
func (s *Snapshot) Node(name string) *corev1.Node {
	for i := range s.Nodes {
		if s.Nodes[i].Name == name {
			return &s.Nodes[i]
		}
	}
	return nil
}

// ClassLister implements structured.DeviceClassLister over the snapshot.
type ClassLister struct{ s *Snapshot }

func (s *Snapshot) ClassLister() ClassLister { return ClassLister{s} }

func (l ClassLister) List() ([]*resourceapi.DeviceClass, error) {
	out := make([]*resourceapi.DeviceClass, 0, len(l.s.DeviceClasses))
	for i := range l.s.DeviceClasses {
		out = append(out, &l.s.DeviceClasses[i])
	}
	return out, nil
}

func (l ClassLister) Get(name string) (*resourceapi.DeviceClass, error) {
	for i := range l.s.DeviceClasses {
		if l.s.DeviceClasses[i].Name == name {
			return &l.s.DeviceClasses[i], nil
		}
	}
	return nil, apierrors.NewNotFound(resourceapi.Resource("deviceclasses"), name)
}

// strip removes fields that are large and irrelevant to allocation, keeping
// snapshot files small enough to attach to a bug report.
func strip(m *metav1.ObjectMeta) {
	m.ManagedFields = nil
	delete(m.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
}

// DeepCopy returns an independent copy, so hypothetical changes never touch
// the original.
func (s *Snapshot) DeepCopy() *Snapshot {
	out := &Snapshot{APIVersion: s.APIVersion, Kind: s.Kind, Metadata: *s.Metadata.DeepCopy()}
	for i := range s.Nodes {
		out.Nodes = append(out.Nodes, *s.Nodes[i].DeepCopy())
	}
	for i := range s.Pods {
		out.Pods = append(out.Pods, *s.Pods[i].DeepCopy())
	}
	for i := range s.DeviceClasses {
		out.DeviceClasses = append(out.DeviceClasses, *s.DeviceClasses[i].DeepCopy())
	}
	for i := range s.ResourceSlices {
		out.ResourceSlices = append(out.ResourceSlices, *s.ResourceSlices[i].DeepCopy())
	}
	for i := range s.ResourceClaims {
		out.ResourceClaims = append(out.ResourceClaims, *s.ResourceClaims[i].DeepCopy())
	}
	for i := range s.DeviceTaintRules {
		out.DeviceTaintRules = append(out.DeviceTaintRules, *s.DeviceTaintRules[i].DeepCopy())
	}
	for i := range s.PodGroups {
		out.PodGroups = append(out.PodGroups, *s.PodGroups[i].DeepCopy())
	}
	return out
}

// DeepCopy copies the metadata.
func (m *Metadata) DeepCopy() *Metadata {
	out := *m
	m.CapturedAt.DeepCopyInto(&out.CapturedAt)
	return &out
}
