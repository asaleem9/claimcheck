package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/render"
)

func newExplain(g *globals) *cobra.Command {
	var output, color string
	var devices bool
	cmd := &cobra.Command{
		Use:   "explain (pod/NAME | resourceclaim/NAME | podgroup/NAME | NAME)",
		Short: "Explain why a pod's ResourceClaims can or can't be allocated",
		Example: `  # Why is this pod Pending?
  kubectl claimcheck explain pod/llm-train -n ml

  # Explain a standalone ResourceClaim, with the per-device table
  kubectl claimcheck explain resourceclaim/shared-gpu --devices

  # Can every member of a gang-scheduled PodGroup get its devices at once?
  kubectl claimcheck explain podgroup/train-gang -n ml

  # Explain offline from a snapshot someone attached to a bug report
  kubectl claimcheck explain pod/llm-train -n ml --from snapshot.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := parseTarget(args[0])
			if err != nil {
				return err
			}
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
			opts := explain.Options{Features: f}
			ns := g.namespace()
			if kind == "podgroup" {
				gang, err := explain.PodGroup(cmd.Context(), snap, ns, name, opts)
				if err != nil {
					return err
				}
				if output == "json" {
					return render.JSONAny(g.out, gang)
				}
				return render.Gang(g.out, gang, render.TextOptions{Style: style, Devices: devices})
			}
			var rep *explain.Report
			if kind == "pod" {
				rep, err = explain.Pod(cmd.Context(), snap, ns, name, opts)
			} else {
				rep, err = explain.ResourceClaim(cmd.Context(), snap, ns, name, opts)
			}
			if err != nil {
				return err
			}
			if output == "json" {
				return render.JSON(g.out, rep)
			}
			return render.Text(g.out, rep, render.TextOptions{Style: style, Devices: devices})
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&devices, "devices", false, "show the per-device table for every eligible node")
	cmd.Flags().StringVar(&color, "color", "auto", "color output: auto, always or never")
	return cmd
}

func parseTarget(arg string) (kind, name string, err error) {
	k, n, ok := strings.Cut(arg, "/")
	if !ok {
		return "pod", arg, nil
	}
	switch strings.ToLower(k) {
	case "pod", "pods", "po":
		return "pod", n, nil
	case "resourceclaim", "resourceclaims":
		return "resourceclaim", n, nil
	case "podgroup", "podgroups", "pg":
		return "podgroup", n, nil
	}
	return "", "", fmt.Errorf("unsupported target %q: use pod/NAME, resourceclaim/NAME or podgroup/NAME", arg)
}

func styleFor(mode string) (render.Style, error) {
	switch mode {
	case "auto":
		return render.AutoStyle(os.Stdout), nil
	case "always":
		return render.Color(), nil
	case "never":
		return render.Plain(), nil
	}
	return render.Style{}, fmt.Errorf("--color must be auto, always or never")
}
