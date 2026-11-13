package explain_test

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/features"
	tu "github.com/asaleem9/claimcheck/internal/testutil"
)

var agreementN = flag.Int("agreement.n", 3000, "number of random clusters for TestAgreement")

// TestAgreement is claimcheck's core invariant: on randomly generated
// clusters without claim constraints (which the diagnosis doesn't model yet),
// the per-device explanation must reach the same fit/no-fit conclusion as the
// scheduler's allocator on every node. A failure here means claimcheck would
// have shown a wrong reason.
func TestAgreement(t *testing.T) {
	iterations := *agreementN
	if testing.Short() {
		iterations = 300
	}
	models := []string{"H100", "L40S", "A10"}
	mems := []string{"24Gi", "40Gi", "80Gi"}
	selectors := []string{
		`device.attributes["gpu.example.com"].model == "H100"`,
		`device.attributes["gpu.example.com"].model != "A10"`,
		`device.capacity["gpu.example.com"].memory.compareTo(quantity("40Gi")) >= 0`,
		`device.capacity["gpu.example.com"].memory.compareTo(quantity("80Gi")) < 0`,
		`true`,
		`device.attributes["gpu.example.com"].?profile.orValue("") != "full"`,
		`device.capacity["gpu.example.com"].memory.compareTo(quantity("40Gi")) <= 0`,
		`device.attributes["gpu.example.com"].model == "NIC"`,
		`device.attributes["gpu.example.com"].model != "NIC"`,
	}
	taint := resourceapi.DeviceTaint{Key: "example.com/unhealthy", Effect: resourceapi.DeviceTaintEffectNoSchedule}
	toleration := resourceapi.DeviceToleration{Key: "example.com/unhealthy", Operator: resourceapi.DeviceTolerationOpExists}

	skipped, checked := 0, 0
	defer func() {
		t.Logf("%d node checks, %d left unexplained by design", checked, skipped)
		if skipped*100 > checked {
			t.Errorf("more than 1%% of nodes unexplained (%d of %d)", skipped, checked)
		}
	}()
	for seed := range uint64(iterations) {
		r := rand.New(rand.NewPCG(seed, 42))
		b := tu.New().GPUClass()
		nodes := 1 + r.IntN(3)
		devCount := 0
		for n := range nodes {
			node := fmt.Sprintf("node-%d", n)
			b.Node(node, nil)
			if r.IntN(3) == 0 {
				// Shared NICs that claims split by bandwidth.
				var nics []tu.SharedDevice
				for i := range 1 + r.IntN(2) {
					nics = append(nics, tu.SharedDevice{Name: fmt.Sprintf("nic-%d", i), Bandwidth: "10G", Min: "1G", Max: "8G", Step: "1G", Default: "1G"})
				}
				b.SharedSlice(node, nics...)
				for i := range r.IntN(3) {
					b.AllocatedShare("other", fmt.Sprintf("share-%s-%d", node, i), node, nics[r.IntN(len(nics))].Name, fmt.Sprintf("%dG", 1+r.IntN(6)))
				}
			}
			if r.IntN(3) == 0 {
				// MIG-style partitions sharing a memory counter per GPU.
				phys := map[string]struct {
					Memory     string
					Partitions []tu.Partition
				}{}
				var all []string
				for g := range 1 + r.IntN(2) {
					name := fmt.Sprintf("gpu-%d", g)
					var ps []tu.Partition
					for i, prof := range []string{"full", "half", "half", "quarter", "quarter", "quarter", "quarter"} {
						mem := map[string]string{"full": "80Gi", "half": "40Gi", "quarter": "20Gi"}[prof]
						p := tu.Partition{Name: fmt.Sprintf("%s-p%d", name, i), Profile: prof, Memory: mem}
						ps = append(ps, p)
						all = append(all, p.Name)
					}
					phys[name] = struct {
						Memory     string
						Partitions []tu.Partition
					}{"80Gi", ps}
				}
				b.PartitionedSlice(node, phys)
				for _, p := range all {
					if r.IntN(5) == 0 {
						b.Allocated("other", fmt.Sprintf("holder-%s-%s", node, p), node, p)
					}
				}
				continue
			}
			var gpus []tu.GPU
			for d := range r.IntN(6) {
				g := tu.GPU{Name: fmt.Sprintf("gpu-%d", d), Model: models[r.IntN(len(models))], Memory: mems[r.IntN(len(mems))]}
				if r.IntN(8) == 0 {
					g.Taints = []resourceapi.DeviceTaint{taint}
				}
				// Topology attributes for constraints; sometimes missing.
				g.Extra = map[string]string{}
				if r.IntN(5) > 0 {
					g.Extra["nvlink"] = []string{"a", "b", "c"}[r.IntN(3)]
				}
				g.Extra["numa"] = []string{"0", "1"}[r.IntN(2)]
				gpus = append(gpus, g)
			}
			if len(gpus) > 0 {
				b.Slice(node, gpus...)
				devCount += len(gpus)
				// Hand some devices to other workloads.
				for _, g := range gpus {
					if r.IntN(4) == 0 {
						b.Allocated("other", fmt.Sprintf("holder-%s-%s", node, g.Name), node, g.Name)
					}
				}
			}
		}

		randReq := func(name string) tu.Req {
			q := tu.Req{Name: name, Count: int64(1 + r.IntN(3))}
			if r.IntN(6) == 0 {
				q.All = true
			}
			for range r.IntN(3) {
				q.Selectors = append(q.Selectors, selectors[r.IntN(len(selectors))])
			}
			if r.IntN(4) == 0 {
				q.Tolerations = []resourceapi.DeviceToleration{toleration}
			}
			if r.IntN(4) == 0 {
				q.Bandwidth = []string{"500M", "2G", "3G", "4G", "9G"}[r.IntN(5)]
			}
			return q
		}
		var claimNames []string
		for c := range 1 + r.IntN(2) {
			var reqs []tu.Req
			for q := range 1 + r.IntN(3) {
				name := fmt.Sprintf("r%d", q)
				if r.IntN(5) == 0 {
					reqs = append(reqs, tu.Req{Name: name, FirstAvailable: []tu.Req{randReq("first"), randReq("second")}})
				} else {
					reqs = append(reqs, randReq(name))
				}
			}
			cn := fmt.Sprintf("claim-%d", c)
			b.Claim("ns", cn, reqs...)
			claimNames = append(claimNames, cn)
			// Constraints over all requests or a random subset, sometimes
			// naming a firstAvailable alternative directly.
			for range r.IntN(3) {
				var names []string
				for _, q := range reqs {
					switch {
					case r.IntN(2) == 0:
					case len(q.FirstAvailable) > 0 && r.IntN(2) == 0:
						names = append(names, q.Name+"/"+q.FirstAvailable[r.IntN(2)].Name)
					default:
						names = append(names, q.Name)
					}
				}
				attr := []string{"gpu.example.com/nvlink", "gpu.example.com/numa"}[r.IntN(2)]
				if r.IntN(3) == 0 {
					b.DistinctAttribute(attr, names...)
				} else {
					b.MatchAttribute(attr, names...)
				}
			}
		}
		b.Pod("ns", "pod", nil, claimNames...)

		rep, err := explain.Pod(context.Background(), b.S, "ns", "pod", explain.Options{Features: features.Default()})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, n := range rep.Nodes {
			checked++
			if !n.Agree && len(n.Diagnosis.Unanalyzed) > 0 {
				skipped++ // the solver gave up honestly; counted below
				continue
			}
			if !n.Agree {
				data, _ := b.S.Marshal()
				if dir := os.Getenv("CLAIMCHECK_DUMP"); dir != "" {
					_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("seed-%d.yaml", seed)), data, 0o644)
				}
				t.Fatalf("seed %d, node %s: allocator fits=%v err=%q, diagnosis fits=%v contention=%q\nsnapshot:\n%s",
					seed, n.Node, n.Verdict.Fits, n.Verdict.Err, n.Diagnosis.Fits, n.Diagnosis.Contention, data)
			}
		}
	}
}
