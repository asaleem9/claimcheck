package whatif

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/features"
	"github.com/asaleem9/claimcheck/internal/snapshot"
)

var opts = explain.Options{Features: features.Default(), Hypothetical: true}

func load(t *testing.T, scenario string) *snapshot.Snapshot {
	t.Helper()
	s, err := snapshot.ReadFile(filepath.Join("..", "..", "testdata", "scenarios", scenario, "snapshot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSuggestFindsTheClaimsInTheWay(t *testing.T) {
	snap := load(t, "all-in-use")
	got, tried, err := Suggest(context.Background(), snap, Target{"pod", "ml", "llm-train"}, opts, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tried != 2 || len(got) != 2 {
		t.Fatalf("tried %d, got %+v; want both holders of the 80Gi GPUs", tried, got)
	}
	for _, s := range got {
		if s.Change.Kind != FreeClaim || len(s.Nodes) != 1 || s.Nodes[0] != "worker-b" {
			t.Errorf("unexpected suggestion %+v", s)
		}
	}
}

func TestSuggestTaintRule(t *testing.T) {
	snap := load(t, "tainted")
	got, _, err := Suggest(context.Background(), snap, Target{"pod", "ml", "llm-train"}, opts, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Change.Kind != RemoveTaintRule || got[0].Change.Name != "maintenance-worker-b" {
		t.Fatalf("got %+v, want removing maintenance-worker-b", got)
	}
}

func TestApplyLeavesOriginalUntouched(t *testing.T) {
	snap := load(t, "all-in-use")
	changed, err := Apply(snap, []Change{{Kind: FreeClaim, Namespace: "ml", Name: "finetune-0-gpu"}})
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := snap.Claim("ml", "finetune-0-gpu")
	freed, _ := changed.Claim("ml", "finetune-0-gpu")
	if orig.Status.Allocation == nil || freed.Status.Allocation != nil {
		t.Fatal("Apply must free the copy and leave the original allocated")
	}
	before, _ := Explain(context.Background(), snap, Target{"pod", "ml", "llm-train"}, opts)
	after, _ := Explain(context.Background(), changed, Target{"pod", "ml", "llm-train"}, opts)
	if before.Status != explain.Blocked || after.Status != explain.Fits {
		t.Fatalf("before %s, after %s; want Blocked then Fits", before.Status, after.Status)
	}
}

func TestReadClaimsFromTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.yaml")
	data := `apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: big
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.example.com
          selectors:
          - cel:
              expression: device.capacity["gpu.example.com"].memory.compareTo(quantity("80Gi")) >= 0
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	changes, err := ReadClaims(path, "ml")
	if err != nil {
		t.Fatal(err)
	}
	snap := load(t, "fits")
	changed, err := Apply(snap, changes)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Explain(context.Background(), changed, Target{"resourceclaim", "ml", "big"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != explain.Fits || len(rep.FitNodes) != 1 || rep.FitNodes[0] != "worker-b" {
		t.Fatalf("got %s on %v (%+v), want Fits on worker-b only", rep.Status, rep.FitNodes, rep.Nodes[1].Verdict)
	}
}
