package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/asaleem9/claimcheck/internal/diagnose"
	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/render"
)

func newPools(g *globals) *cobra.Command {
	var output, color, node string
	var devices bool
	cmd := &cobra.Command{
		Use:   "pools",
		Short: "Show free, used, tainted and fragmented devices in every pool",
		Long: `pools shows each device pool the way the allocator sees it for a new claim:
devices in use, shared, tainted, blocked because their shared counters are used
up by sibling partitions, or free. Partitioned GPUs show their counters with the
largest partition that still fits, and shared devices show their capacity.`,
		Example: `  kubectl claimcheck pools
  kubectl claimcheck pools --node gpu-node-3 --devices
  kubectl claimcheck pools --from snapshot.yaml -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := g.featureSet()
			if err != nil {
				return err
			}
			style, err := styleFor(color)
			if err != nil {
				return err
			}
			if output != "text" && output != "json" {
				return fmt.Errorf("--output must be text or json")
			}
			snap, err := g.load(cmd.Context())
			if err != nil {
				return err
			}
			dg := explain.NewDiagnoser(snap, f, sets.New[types.UID]())
			pools := dg.Inventory(cmd.Context())
			if node != "" {
				var kept []diagnose.PoolView
				for _, p := range pools {
					if p.Node == node {
						kept = append(kept, p)
					}
				}
				pools = kept
			}
			rep := render.PoolsReport{Context: snap.Metadata.Context, Nodes: len(snap.Nodes), Pools: pools}
			if output == "json" {
				return render.PoolsJSON(g.out, rep)
			}
			return render.Pools(g.out, rep, render.TextOptions{Style: style, Devices: devices})
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&devices, "devices", false, "list every device with its holder or reason")
	cmd.Flags().StringVar(&node, "node", "", "only show pools of this node")
	cmd.Flags().StringVar(&color, "color", "auto", "color output: auto, always or never")
	return cmd
}
