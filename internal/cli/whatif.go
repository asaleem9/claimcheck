package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asaleem9/claimcheck/internal/explain"
	"github.com/asaleem9/claimcheck/internal/render"
	"github.com/asaleem9/claimcheck/internal/whatif"
)

func newWhatIf(g *globals) *cobra.Command {
	var output, color, file string
	var free, removeRules []string
	var devices, suggest bool
	var limit int
	cmd := &cobra.Command{
		Use:   "whatif [pod/NAME | resourceclaim/NAME]",
		Short: "Explain a pod or claim against a hypothetically changed cluster",
		Long: `whatif applies changes to a copy of the cluster state and explains the pod or
claim again, showing the result before and after. Nothing in the cluster changes.

With --suggest it tries every single change that could help (freeing each
allocated claim, removing each DeviceTaintRule) and lists the ones that would
let it fit.`,
		Example: `  # Would llm-train fit if the finetune job's GPUs were released?
  kubectl claimcheck whatif pod/llm-train -n ml --free pod/finetune

  # Which single change would let it fit?
  kubectl claimcheck whatif pod/llm-train -n ml --suggest

  # Would this claim fit right now, and where?
  kubectl claimcheck whatif -f claim.yaml -n ml`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			ns := g.namespace()
			var changes []whatif.Change
			if file != "" {
				added, err := whatif.ReadClaims(file, ns)
				if err != nil {
					return err
				}
				changes = append(changes, added...)
			}
			for _, fr := range free {
				kind, name, err := parseTarget(fr)
				if err != nil {
					return fmt.Errorf("--free: %w", err)
				}
				k := whatif.FreePod
				switch kind {
				case "resourceclaim":
					k = whatif.FreeClaim
				case "podgroup":
					return fmt.Errorf("--free takes pod/NAME or resourceclaim/NAME")
				}
				changes = append(changes, whatif.Change{Kind: k, Namespace: ns, Name: name})
			}
			for _, r := range removeRules {
				changes = append(changes, whatif.Change{Kind: whatif.RemoveTaintRule, Name: r})
			}

			var t whatif.Target
			switch {
			case len(args) == 1:
				kind, name, err := parseTarget(args[0])
				if err != nil {
					return err
				}
				if kind == "podgroup" {
					return fmt.Errorf("whatif works on pods and claims; use explain podgroup/%s", name)
				}
				t = whatif.Target{Kind: kind, Namespace: ns, Name: name}
			case file != "" && len(changes) > 0 && changes[0].Kind == whatif.AddClaim:
				t = whatif.Target{Kind: "resourceclaim", Namespace: changes[0].Namespace, Name: changes[0].Name}
			default:
				return fmt.Errorf("name a pod/NAME or resourceclaim/NAME, or pass -f with a claim")
			}
			if len(changes) == 0 && !suggest {
				return fmt.Errorf("nothing to change: use --free, --remove-taint-rule, -f or --suggest")
			}

			snap, err := g.load(cmd.Context())
			if err != nil {
				return err
			}
			opts := explain.Options{Features: f}
			text := render.TextOptions{Style: style, Devices: devices}
			hypo := explain.Options{Features: f, Hypothetical: true}

			if suggest {
				base, err := whatif.Apply(snap, changes)
				if err != nil {
					return err
				}
				before, err := whatif.Explain(cmd.Context(), base, t, hypo)
				if err != nil {
					return err
				}
				rep := render.SuggestReport{Target: before.Target, Status: before.Status}
				if before.Status != explain.Fits && before.Status != explain.Allocated {
					rep.Suggestions, rep.Tried, err = whatif.Suggest(cmd.Context(), base, t, hypo, limit)
					if err != nil {
						return err
					}
				}
				if output == "json" {
					return render.JSONAny(g.out, rep)
				}
				return render.Suggest(g.out, rep, text)
			}

			// An added claim doesn't exist before the change, so "before" is
			// only meaningful for existing targets.
			var before *explain.Report
			newClaim := t.Kind == "resourceclaim" && addsClaim(changes, t)
			if !newClaim {
				if before, err = whatif.Explain(cmd.Context(), snap, t, opts); err != nil {
					return err
				}
			}
			changed, err := whatif.Apply(snap, changes)
			if err != nil {
				return err
			}
			after, err := whatif.Explain(cmd.Context(), changed, t, hypo)
			if err != nil {
				return err
			}
			if before == nil {
				if output == "json" {
					return render.JSON(g.out, after)
				}
				return render.Text(g.out, after, text)
			}
			rep := render.WhatIfReport{Changes: changes, Before: before, After: after}
			if output == "json" {
				return render.JSONAny(g.out, rep)
			}
			return render.WhatIf(g.out, rep, text)
		},
	}
	cmd.Flags().StringArrayVar(&free, "free", nil, "pretend a claim (resourceclaim/NAME) or all claims of a pod (pod/NAME) are released; repeatable")
	cmd.Flags().StringArrayVar(&removeRules, "remove-taint-rule", nil, "pretend a DeviceTaintRule is deleted; repeatable")
	cmd.Flags().StringVarP(&file, "filename", "f", "", "add ResourceClaims or ResourceClaimTemplates from a YAML file")
	cmd.Flags().BoolVar(&suggest, "suggest", false, "find single changes that would make it fit")
	cmd.Flags().IntVar(&limit, "suggest-limit", 200, "maximum number of changes --suggest tries")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	cmd.Flags().BoolVar(&devices, "devices", false, "show the per-device table for every eligible node")
	cmd.Flags().StringVar(&color, "color", "auto", "color output: auto, always or never")
	return cmd
}

func addsClaim(changes []whatif.Change, t whatif.Target) bool {
	for _, c := range changes {
		if c.Kind == whatif.AddClaim && c.Namespace == t.Namespace && strings.EqualFold(c.Name, t.Name) {
			return true
		}
	}
	return false
}
