package explain_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

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
	name     string
	build    func() *tu.B
	features string
	status   explain.Status
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
		name: "match-attribute",
		build: func() *tu.B {
			return nvlinkNode().Allocated("ml", "other", "worker", "gpu-1").
				Claim("ml", "nvlinked", tu.Req{Name: "a"}, tu.Req{Name: "b"}).
				MatchAttribute("gpu.example.com/nvlink", "a", "b").
				Pod("ml", "tp-train", nil, "nvlinked")
		},
		status: explain.Blocked,
	},
	{
		name: "match-attribute-fits",
		build: func() *tu.B {
			return nvlinkNode().Allocated("ml", "other", "worker", "gpu-0").
				Claim("ml", "nvlinked", tu.Req{Name: "a"}, tu.Req{Name: "b"}).
				MatchAttribute("gpu.example.com/nvlink", "a", "b").
				Pod("ml", "tp-train", nil, "nvlinked")
		},
		status: explain.Fits,
	},
	{
		name: "distinct-attribute",
		build: func() *tu.B {
			return nvlinkNode().Claim("ml", "spread", tu.Req{Name: "gpus", Count: 3}).
				DistinctAttribute("gpu.example.com/nvlink").
				Pod("ml", "spread-train", nil, "spread")
		},
		status: explain.Blocked,
	},
	{
		name: "derived-attribute",
		build: func() *tu.B {
			return nvlinkNode().Claim("ml", "nvlinked", tu.Req{Name: "a", Derived: map[string]string{"example.com/fabric": `"fabric-" + device.attributes["gpu.example.com"].model`}},
				tu.Req{Name: "b", Derived: map[string]string{"example.com/fabric": `device.attributes["gpu.example.com"].?nvlink.orValue("none")`}}).
				MatchAttribute("example.com/fabric", "a", "b").
				Pod("ml", "tp-train", nil, "nvlinked")
		},
		features: "DRADerivedAttributes=true",
		status:   explain.Blocked,
	},
	{
		name: "partition-exhausted",
		build: func() *tu.B {
			return partitionedNode().
				Allocated("ml", "inference", "worker", "gpu-0-half-0").
				Allocated("ml", "notebook", "worker", "gpu-1-quarter-2").
				Claim("ml", "full-gpu", tu.Req{Name: "gpu", Selectors: []string{`device.attributes["gpu.example.com"].profile == "full"`}}).
				Pod("ml", "train", nil, "full-gpu")
		},
		status: explain.Blocked,
	},
	{
		name: "partition-joint",
		build: func() *tu.B {
			return partitionedNode().
				Allocated("ml", "notebook", "worker", "gpu-0-quarter-0").
				Allocated("ml", "inference", "worker", "gpu-1-full").
				Claim("ml", "two-halves", tu.Req{Name: "halves", Count: 2, Selectors: []string{`device.attributes["gpu.example.com"].profile == "half"`}}).
				MatchAttribute("gpu.example.com/parent").
				Pod("ml", "train", nil, "two-halves")
		},
		status: explain.Blocked,
	},
	{
		name: "partition-fits",
		build: func() *tu.B {
			return partitionedNode().
				Allocated("ml", "notebook", "worker", "gpu-0-quarter-0").
				Claim("ml", "two-halves", tu.Req{Name: "halves", Count: 2, Selectors: []string{`device.attributes["gpu.example.com"].profile == "half"`}}).
				MatchAttribute("gpu.example.com/parent").
				Pod("ml", "train", nil, "two-halves")
		},
		status: explain.Fits,
	},
	{
		name: "list-attribute",
		build: func() *tu.B {
			b := tu.New().Node("worker", nil).GPUClass().Slice("worker",
				tu.GPU{Name: "gpu-0", Model: "H100", Memory: "80Gi", Lists: map[string][]string{"links": {"a", "b"}}},
				tu.GPU{Name: "gpu-1", Model: "H100", Memory: "80Gi", Lists: map[string][]string{"links": {"b", "c"}}},
				tu.GPU{Name: "gpu-2", Model: "H100", Memory: "80Gi", Lists: map[string][]string{"links": {"c", "d"}}},
			)
			return b.Claim("ml", "linked", tu.Req{Name: "gpus", Count: 3}).
				MatchAttribute("gpu.example.com/links").
				Pod("ml", "tp-train", nil, "linked")
		},
		features: "DRAListTypeAttributes=true",
		status:   explain.Blocked,
	},
	{
		name: "shared-capacity-used-up",
		build: func() *tu.B {
			return sharedNIC().
				AllocatedShare("serving", "llm-a-net", "worker", "nic-0", "4G").
				AllocatedShare("serving", "llm-b-net", "worker", "nic-0", "4G").
				Claim("ml", "net", tu.Req{Name: "net", Bandwidth: "4G", Selectors: []string{nic}}).
				Pod("ml", "trainer", nil, "net")
		},
		status: explain.Blocked,
	},
	{
		name: "shared-capacity-policy",
		build: func() *tu.B {
			return sharedNIC().
				Claim("ml", "net", tu.Req{Name: "net", Bandwidth: "9G", Selectors: []string{nic}}).
				Pod("ml", "trainer", nil, "net")
		},
		status: explain.Blocked,
	},
	{
		name: "shared-capacity-fits",
		build: func() *tu.B {
			return sharedNIC().
				AllocatedShare("serving", "llm-a-net", "worker", "nic-0", "4G").
				Claim("ml", "net", tu.Req{Name: "ingest", Bandwidth: "3G", Selectors: []string{nic}}, tu.Req{Name: "sync", Bandwidth: "3G", Selectors: []string{nic}}).
				Pod("ml", "trainer", nil, "net")
		},
		status: explain.Fits,
	},
}

const nic = `device.attributes["gpu.example.com"].model == "NIC"`

// sharedNIC has one worker with a 10G NIC that claims share in 1G steps,
// up to 8G each.
func sharedNIC() *tu.B {
	return tu.New().Node("worker", nil).GPUClass().
		SharedSlice("worker", tu.SharedDevice{Name: "nic-0", Bandwidth: "10G", Min: "1G", Max: "8G", Step: "1G", Default: "1G"})
}

// partitionedNode has one worker with two 80Gi GPUs, each published as one
// full partition, two halves and four quarters sharing a memory counter.
func partitionedNode() *tu.B {
	parts := func(phys string) []tu.Partition {
		ps := []tu.Partition{{Name: phys + "-full", Profile: "full", Memory: "80Gi"}}
		for i := range 2 {
			ps = append(ps, tu.Partition{Name: fmt.Sprintf("%s-half-%d", phys, i), Profile: "half", Memory: "40Gi"})
		}
		for i := range 4 {
			ps = append(ps, tu.Partition{Name: fmt.Sprintf("%s-quarter-%d", phys, i), Profile: "quarter", Memory: "20Gi"})
		}
		return ps
	}
	return tu.New().Node("worker", nil).GPUClass().PartitionedSlice("worker", map[string]struct {
		Memory     string
		Partitions []tu.Partition
	}{
		"gpu-0": {Memory: "80Gi", Partitions: parts("gpu-0")},
		"gpu-1": {Memory: "80Gi", Partitions: parts("gpu-1")},
	})
}

// nvlinkNode has one worker with two NVLink domains: gpu-0 alone in
// domain-a, gpu-1 and gpu-2 in domain-b, and gpu-3 with no NVLink.
func nvlinkNode() *tu.B {
	return tu.New().Node("worker", nil).GPUClass().Slice("worker",
		tu.GPU{Name: "gpu-0", Model: "H100", Memory: "80Gi", Extra: map[string]string{"nvlink": "domain-a"}},
		tu.GPU{Name: "gpu-1", Model: "H100", Memory: "80Gi", Extra: map[string]string{"nvlink": "domain-b"}},
		tu.GPU{Name: "gpu-2", Model: "H100", Memory: "80Gi", Extra: map[string]string{"nvlink": "domain-b"}},
		tu.GPU{Name: "gpu-3", Model: "H100", Memory: "80Gi"},
	)
}

func TestScenarios(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			snap := sc.build().S
			pod := snap.Pods[0]
			f, err := features.Parse(sc.features)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := explain.Pod(context.Background(), snap, pod.Namespace, pod.Name, explain.Options{Features: f})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != sc.status {
				for _, n := range rep.Nodes {
					t.Logf("node %s verdict=%+v", n.Node, n.Verdict)
				}
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
			// The pools view of the same cluster.
			var pools bytes.Buffer
			inv := explain.NewDiagnoser(snap, f, sets.New[types.UID]()).Inventory(context.Background())
			if err := render.Pools(&pools, render.PoolsReport{Nodes: len(snap.Nodes), Pools: inv}, render.TextOptions{Style: render.Plain(), Devices: true}); err != nil {
				t.Fatal(err)
			}

			dir := filepath.Join("..", "..", "testdata", "scenarios", sc.name)
			golden := filepath.Join(dir, "explain.golden")
			poolsGolden := filepath.Join(dir, "pools.golden")
			if *update {
				data, err := snap.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				must(t, os.MkdirAll(dir, 0o755))
				must(t, os.WriteFile(filepath.Join(dir, "snapshot.yaml"), data, 0o644))
				must(t, os.WriteFile(golden, buf.Bytes(), 0o644))
				must(t, os.WriteFile(poolsGolden, pools.Bytes(), 0o644))
				return
			}
			wantPools, err := os.ReadFile(poolsGolden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/explain -update)", err)
			}
			if !bytes.Equal(wantPools, pools.Bytes()) {
				t.Errorf("pools output differs from %s (run with -update to accept):\n%s", poolsGolden, pools.String())
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

var l40s = `device.attributes["gpu.example.com"].model == "L40S"`

// gang returns a PodGroup of n members that each need two L40S GPUs. The
// standard cluster has six L40S (four on worker-a, two on worker-b).
func gang(n int, minCount int32, groupClaims ...string) func(b *tu.B) *tu.B {
	return func(b *tu.B) *tu.B {
		b.PodGroup("ml", "train-gang", minCount, groupClaims...)
		for i := range n {
			claim := fmt.Sprintf("train-%d-gpus", i)
			b.Claim("ml", claim, tu.Req{Name: "gpus", Count: 2, Selectors: []string{l40s}})
			b.Member("ml", "train-gang", fmt.Sprintf("train-%d", i), claim)
		}
		return b
	}
}

var gangScenarios = []struct {
	name   string
	build  func() *tu.B
	status explain.Status
}{
	{"gang-blocked", func() *tu.B { return gang(4, 4)(tu.Standard()) }, explain.Blocked},
	{"gang-fits", func() *tu.B { return gang(4, 3)(tu.Standard()) }, explain.Fits},
	{"gang-group-claim", func() *tu.B {
		b := tu.Standard().Claim("ml", "shared-h100", tu.Req{Name: "gpu", Selectors: []string{h100}})
		return gang(2, 2, "shared-h100")(b)
	}, explain.Blocked},
}

func TestGangScenarios(t *testing.T) {
	for _, sc := range gangScenarios {
		t.Run(sc.name, func(t *testing.T) {
			snap := sc.build().S
			rep, err := explain.PodGroup(context.Background(), snap, "ml", "train-gang", explain.Options{Features: features.Default()})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != sc.status {
				t.Errorf("status = %s, want %s: %s", rep.Status, sc.status, rep.Summary)
			}
			for _, m := range rep.Members {
				if m.Report == nil {
					continue
				}
				for _, n := range m.Report.Nodes {
					if !n.Agree && n.Excluded == "" {
						t.Errorf("member %s node %s: explanation disagrees with allocator", m.Pod, n.Node)
					}
				}
			}
			var buf bytes.Buffer
			if err := render.Gang(&buf, rep, render.TextOptions{Style: render.Plain()}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("..", "..", "testdata", "scenarios", sc.name)
			golden := filepath.Join(dir, "explain.golden")
			if *update {
				data, err := snap.Marshal()
				must(t, err)
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
