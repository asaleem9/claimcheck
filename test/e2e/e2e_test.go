//go:build e2e

// Package e2e checks claimcheck against a real scheduler: each scenario makes
// kube-scheduler reject (or accept) a workload, then asserts that claimcheck
// reaches the same verdict and names the expected reason.
//
// Run with: hack/kind/up.sh && go test -tags e2e ./test/e2e -v
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/features"
	"github.com/asaleem9/claimcheck/internal/snapshot"
)

type header struct {
	kind, namespace, name string
	status                explain.Status
	reason                string
}

// TestScenarios runs against whole GPUs (hack/kind/up.sh).
func TestScenarios(t *testing.T) {
	cs := client(t)
	if partitioned(t, cs) {
		t.Skip("cluster publishes partitioned GPUs; these scenarios need whole GPUs")
	}
	run(t, cs, "scenarios")
}

// TestPartitionScenarios runs against partitioned GPUs
// (GPU_PARTITIONS=4 hack/kind/up.sh).
func TestPartitionScenarios(t *testing.T) {
	cs := client(t)
	if !partitioned(t, cs) {
		t.Skip("cluster publishes whole GPUs; rerun hack/kind/up.sh with GPU_PARTITIONS=4")
	}
	run(t, cs, "partitions")
}

// TestGangScenarios runs against a cluster serving the PodGroup API
// (KIND_CONFIG=hack/kind/kind-gang.yaml hack/kind/up.sh).
func TestGangScenarios(t *testing.T) {
	cs := client(t)
	if _, err := cs.SchedulingV1beta1().PodGroups("default").List(context.Background(), metav1.ListOptions{Limit: 1}); err != nil {
		t.Skipf("PodGroup API not served (%v); create the cluster with KIND_CONFIG=hack/kind/kind-gang.yaml", err)
	}
	run(t, cs, "gangs")
}

func client(t *testing.T) kubernetes.Interface {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return kubernetes.NewForConfigOrDie(cfg)
}

// partitioned reports whether the driver publishes shared counters.
func partitioned(t *testing.T, cs kubernetes.Interface) bool {
	t.Helper()
	slices, err := cs.ResourceV1().ResourceSlices().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slices.Items {
		if len(s.Spec.SharedCounters) > 0 {
			return true
		}
	}
	return false
}

func run(t *testing.T, cs kubernetes.Interface, set string) {
	dirs, err := filepath.Glob(set + "/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			h := readHeader(t, filepath.Join(dir, "test.yaml"))

			setup := filepath.Join(dir, "setup.yaml")
			if _, err := os.Stat(setup); err == nil {
				kubectl(t, "apply", "-f", setup)
				t.Cleanup(func() { kubectlQuiet("delete", "-f", setup, "--ignore-not-found", "--wait") })
				waitPodsScheduled(ctx, t, cs, h.namespace)
			}
			kubectl(t, "apply", "-f", filepath.Join(dir, "test.yaml"))
			t.Cleanup(func() { kubectlQuiet("delete", "-f", filepath.Join(dir, "test.yaml"), "--ignore-not-found", "--wait") })

			switch h.kind {
			case "pod":
				waitUnschedulable(ctx, t, cs, h.namespace, h.name)
			case "podgroup":
				waitGroupPending(ctx, t, cs, h.namespace)
			}

			snap, err := snapshot.FromCluster(ctx, cs, "e2e")
			if err != nil {
				t.Fatal(err)
			}
			opts := explain.Options{Features: features.Default()}
			if h.kind == "podgroup" {
				gang, err := explain.PodGroup(ctx, snap, h.namespace, h.name, opts)
				if err != nil {
					t.Fatal(err)
				}
				out, _ := json.MarshalIndent(gang, "", "  ")
				if gang.Status != h.status {
					t.Fatalf("status = %s, want %s\n%s", gang.Status, h.status, out)
				}
				if h.reason != "" && !strings.Contains(gang.Summary+"\n"+string(out), h.reason) {
					t.Errorf("report does not mention %q:\n%s", h.reason, out)
				}
				t.Logf("%s: %s", gang.Status, gang.Summary)
				return
			}
			var rep *explain.Report
			if h.kind == "pod" {
				rep, err = explain.Pod(ctx, snap, h.namespace, h.name, opts)
			} else {
				rep, err = explain.ResourceClaim(ctx, snap, h.namespace, h.name, opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			out, _ := json.MarshalIndent(rep, "", "  ")
			if rep.Status != h.status {
				t.Fatalf("status = %s, want %s\n%s", rep.Status, h.status, out)
			}
			for _, n := range rep.Nodes {
				if n.Excluded == "" && !n.Agree {
					t.Errorf("node %s: explanation disagrees with allocator", n.Node)
				}
			}
			// Search the JSON (for reason codes) and the plain hints (for
			// messages with quotes, which JSON escapes).
			haystack := string(out) + "\n" + strings.Join(rep.Hints, "\n")
			if h.reason != "" && !strings.Contains(haystack, h.reason) {
				t.Errorf("report does not mention %q:\n%s", h.reason, out)
			}
			t.Logf("%s: %s", rep.Status, rep.Summary)
			for _, hint := range rep.Hints {
				t.Logf("  %s", hint)
			}
		})
	}
}

func readHeader(t *testing.T, path string) header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var h header
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#") {
			break
		}
		k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "target":
			h.kind, h.name, _ = strings.Cut(v, "/")
		case "namespace":
			h.namespace = v
		case "expect-status":
			h.status = explain.Status(v)
		case "expect-reason":
			h.reason = v
		}
	}
	if h.kind == "" || h.status == "" || h.namespace == "" {
		t.Fatalf("%s: incomplete header %+v", path, h)
	}
	return h
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func kubectlQuiet(args ...string) { _ = exec.Command("kubectl", args...).Run() }

func waitPodsScheduled(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	poll(ctx, t, "setup pods scheduled", func() bool {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == "" {
				return false
			}
		}
		return true
	})
}

// waitUnschedulable waits until kube-scheduler has tried and failed to place
// the pod, so the snapshot shows the state the scheduler rejected.
func waitUnschedulable(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns, name string) {
	t.Helper()
	poll(ctx, t, "pod "+name+" unschedulable", func() bool {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) || err != nil {
			return false
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				t.Logf("scheduler: %s", c.Message)
				return true
			}
		}
		if p.Spec.NodeName != "" {
			t.Fatalf("pod %s was scheduled to %s, but the scenario expects it to be rejected", name, p.Spec.NodeName)
		}
		return false
	})
}

// waitGroupPending waits until the scheduler has tried and rejected every
// member pod of the namespace's gang.
func waitGroupPending(ctx context.Context, t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	poll(ctx, t, "gang members unschedulable", func() bool {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil || len(pods.Items) == 0 {
			return false
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName != "" {
				t.Fatalf("member %s was scheduled to %s, but the gang should not fit", p.Name, p.Spec.NodeName)
			}
			pending := false
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
					pending = true
				}
			}
			if !pending {
				return false
			}
		}
		t.Logf("scheduler: %s", pods.Items[0].Status.Conditions[0].Message)
		return true
	})
}

func poll(ctx context.Context, t *testing.T, what string, done func() bool) {
	t.Helper()
	for {
		if done() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Second):
		}
	}
}
