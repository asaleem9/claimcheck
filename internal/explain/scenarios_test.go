package explain_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/features"
	"github.com/asaleem9/claimcheck/internal/render"
	tu "github.com/asaleem9/claimcheck/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files and scenario snapshots")

const (
	mem80 = `device.capacity["gpu.example.com"].memory.compareTo(quantity("80Gi")) >= 0`
	h100  = `device.attributes["gpu.example.com"].model == "H100"`
)

type scenario struct {
	name   string
	build  func() *tu.B
	status explain.Status
}

var scenarios = []scenario{
	{
		name: "fits",
		build: func() *tu.B {
			return tu.Standard().Claim("ml", "train-gpu", tu.Req{Name: "gpu"}).Pod("ml", "train", nil, "train-gpu")
		},
		status: explain.Fits,
	},
	{
		name: "selector-mismatch",
		build: func() *tu.B {
			return tu.Standard().
				Claim("ml", "big-gpu", tu.Req{Name: "gpu", Selectors: []string{`device.capacity["gpu.example.com"].memory.compareTo(quantity("120Gi")) >= 0`}}).
				Pod("ml", "llm-train", nil, "big-gpu")
		},
		status: explain.Blocked,
	},
	{
		name: "all-in-use",
		build: func() *tu.B {
			return tu.Standard().
				Allocated("ml", "finetune-0-gpu", "worker-b", "gpu-0").
				Allocated("research", "notebook-gpu", "worker-b", "gpu-1").
				Claim("ml", "llm-train-gpu", tu.Req{Name: "gpu", Selectors: []string{mem80}}).
				Pod("ml", "llm-train", nil, "llm-train-gpu")
		},
		status: explain.Blocked,
	},
	{
		name: "tainted",
		build: func() *tu.B {
			return tu.Standard().
				TaintRule("maintenance-worker-b", "worker-b", resourceapi.DeviceTaint{Key: "example.com/maintenance", Value: "driver-upgrade", Effect: resourceapi.DeviceTaintEffectNoSchedule}).
				Claim("ml", "llm-train-gpu", tu.Req{Name: "gpu", Selectors: []string{h100}}).
				Pod("ml", "llm-train", nil, "llm-train-gpu")
		},
		status: explain.Blocked,
	},
	{
		name: "tolerated-taint",
		build: func() *tu.B {
			return tu.Standard().
				TaintRule("maintenance-worker-b", "worker-b", resourceapi.DeviceTaint{Key: "example.com/maintenance", Value: "driver-upgrade", Effect: resourceapi.DeviceTaintEffectNoSchedule}).
				Claim("ml", "llm-train-gpu", tu.Req{Name: "gpu", Selectors: []string{h100}, Tolerations: []resourceapi.DeviceToleration{{
					Key: "example.com/maintenance", Operator: resourceapi.DeviceTolerationOpExists, Effect: resourceapi.DeviceTaintEffectNoSchedule,
				}}}).
				Pod("ml", "llm-train", nil, "llm-train-gpu")
		},
		status: explain.Fits,
	},
	{
		name: "class-missing",
		build: func() *tu.B {
			return tu.Standard().Claim("ml", "tpu", tu.Req{Name: "tpu", Class: "tpu.example.com"}).Pod("ml", "jax", nil, "tpu")
		},
		status: explain.AllocatorError,
	},
	{
		name: "count-too-high",
		build: func() *tu.B {
			return tu.Standard().Claim("ml", "eight-gpus", tu.Req{Name: "gpus", Count: 6}).Pod("ml", "ddp", nil, "eight-gpus")
		},
		status: explain.Blocked,
	},
	{
		name: "contention",
		build: func() *tu.B {
			return tu.Standard().
				Allocated("ml", "other", "worker-b", "gpu-1").
				Claim("ml", "pair", tu.Req{Name: "trainer", Selectors: []string{h100}}, tu.Req{Name: "evaluator", Selectors: []string{h100}}).
				Pod("ml", "train-and-eval", map[string]string{"pool": "training"}, "pair")
		},
		status: explain.Blocked,
	},
	{
		name: "node-selector",
		build: func() *tu.B {
			return tu.Standard().
				Claim("ml", "llm-train-gpu", tu.Req{Name: "gpu", Selectors: []string{mem80}}).
				Pod("ml", "llm-train", map[string]string{"pool": "general"}, "llm-train-gpu")
		},
		status: explain.Blocked,
	},
	{
		name: "selector-error",
		build: func() *tu.B {
			return tu.Standard().
				Claim("ml", "gpu", tu.Req{Name: "gpu", Selectors: []string{`device.attributes["gpu.example.com"].architecture == "Hopper"`}}).
				Pod("ml", "train", nil, "gpu")
		},
		status: explain.AllocatorError,
	},
	{
		name: "first-available",
		build: func() *tu.B {
			return tu.Standard().
				Allocated("ml", "a", "worker-b", "gpu-0", "gpu-1").
				Claim("ml", "flex", tu.Req{Name: "gpu", FirstAvailable: []tu.Req{
					{Name: "h100", Selectors: []string{h100}},
					{Name: "two-l40s", Count: 5, Selectors: []string{`device.attributes["gpu.example.com"].model == "L40S"`}},
				}}).
				Pod("ml", "flex-train", nil, "flex")
		},
		status: explain.Blocked,
	},
	{
		name: "match-attribute-unexplained",
		build: func() *tu.B {
			b := tu.New().Node("worker", nil).GPUClass().Slice("worker",
				tu.GPU{Name: "gpu-0", Model: "H100", Memory: "80Gi", Extra: map[string]string{"nvlink": "domain-a"}},
				tu.GPU{Name: "gpu-1", Model: "H100", Memory: "80Gi", Extra: map[string]string{"nvlink": "domain-b"}},
			)
			return b.Claim("ml", "nvlinked", tu.Req{Name: "a"}, tu.Req{Name: "b"}).
				MatchAttribute("gpu.example.com/nvlink", "a", "b").
				Pod("ml", "tp-train", nil, "nvlinked")
		},
		status: explain.Unexplained,
	},
}

func TestScenarios(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			snap := sc.build().S
			pod := snap.Pods[0]
			rep, err := explain.Pod(context.Background(), snap, pod.Namespace, pod.Name, explain.Options{Features: features.Default()})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != sc.status {
				t.Errorf("status = %s, want %s\nsummary: %s\nhints: %q", rep.Status, sc.status, rep.Summary, rep.Hints)
			}
			// Every eligible node must agree unless the scenario is about disagreement.
			for _, n := range rep.Nodes {
				if !n.Agree && n.Excluded == "" && sc.status != explain.Unexplained {
					t.Errorf("node %s: diagnosis (fits=%v) disagrees with allocator (fits=%v, err=%q)", n.Node, n.Diagnosis.Fits, n.Verdict.Fits, n.Verdict.Err)
				}
			}

			var buf bytes.Buffer
			if err := render.Text(&buf, rep, render.TextOptions{Style: render.Plain(), Devices: true}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("..", "..", "testdata", "scenarios", sc.name)
			golden := filepath.Join(dir, "explain.golden")
			if *update {
				data, err := snap.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				must(t, os.MkdirAll(dir, 0o755))
				must(t, os.WriteFile(filepath.Join(dir, "snapshot.yaml"), data, 0o644))
				must(t, os.WriteFile(golden, buf.Bytes(), 0o644))
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/explain -update)", err)
			}
			if !bytes.Equal(want, buf.Bytes()) {
				t.Errorf("output differs from %s (run with -update to accept):\n%s", golden, buf.String())
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
