// Package cli wires claimcheck's commands into a kubectl plugin.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/structured"

	"github.com/asaleem9/claimcheck/internal/features"
	"github.com/asaleem9/claimcheck/internal/snapshot"
)

// Version is set at build time with -ldflags.
var Version = "dev"

type globals struct {
	config   *genericclioptions.ConfigFlags
	from     string
	features string
	out      io.Writer
}

// NewRoot builds the `kubectl claimcheck` command tree.
func NewRoot() *cobra.Command {
	g := &globals{config: genericclioptions.NewConfigFlags(true), out: os.Stdout}
	root := &cobra.Command{
		Use:   "kubectl-claimcheck",
		Short: "Explain why Kubernetes can't allocate devices (DRA) to your pod",
		Long: `claimcheck replays the scheduler's own DRA allocator against your cluster
and explains, device by device, why a ResourceClaim can't be allocated.

Every verdict comes from the same allocator kube-scheduler runs. The
explanation is checked against it; when the two disagree, claimcheck
says so instead of guessing.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	g.config.AddFlags(root.PersistentFlags())
	root.PersistentFlags().StringVar(&g.from, "from", "", "read cluster state from a snapshot file instead of the API server")
	root.PersistentFlags().StringVar(&g.features, "features", "", "override DRA feature gates, e.g. DRADerivedAttributes=true,DRAConsumableCapacity=false")

	root.AddCommand(newExplain(g), newWhatIf(g), newPools(g), newSnapshot(g), newVersion(g))
	return root
}

func (g *globals) featureSet() (structured.Features, error) {
	return features.Parse(g.features)
}

// namespace returns -n, or the kubeconfig context's namespace.
func (g *globals) namespace() string {
	if g.config.Namespace != nil && *g.config.Namespace != "" {
		return *g.config.Namespace
	}
	if ns, _, err := g.config.ToRawKubeConfigLoader().Namespace(); err == nil && ns != "" {
		return ns
	}
	return "default"
}

func (g *globals) load(ctx context.Context) (*snapshot.Snapshot, error) {
	if g.from != "" {
		return snapshot.ReadFile(g.from)
	}
	cfg, err := g.config.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	contextName := ""
	if raw, err := g.config.ToRawKubeConfigLoader().RawConfig(); err == nil {
		contextName = raw.CurrentContext
	}
	if g.config.Context != nil && *g.config.Context != "" {
		contextName = *g.config.Context
	}
	return snapshot.FromCluster(ctx, cs, contextName)
}

func newVersion(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the claimcheck version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := Version
			if bi, ok := debug.ReadBuildInfo(); ok && v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				v = strings.TrimPrefix(bi.Main.Version, "v") // go install ...@vX.Y.Z
			}
			_, err := fmt.Fprintf(g.out, "claimcheck %s (allocator k8s.io/dynamic-resource-allocation v0.37.2)\n", v)
			return err
		},
	}
}
