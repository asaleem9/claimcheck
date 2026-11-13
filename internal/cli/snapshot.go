package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newSnapshot(g *globals) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Save the cluster's DRA state to a file for offline explain runs",
		Long: `snapshot captures nodes, pods using DRA, DeviceClasses, ResourceSlices,
ResourceClaims and DeviceTaintRules. Attach it to a bug report so a driver or
Kubernetes maintainer can reproduce your allocation problem with --from.

Review the file before sharing it: it contains node labels and object names.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.from != "" {
				return fmt.Errorf("snapshot reads from the cluster; --from is not supported here")
			}
			snap, err := g.load(cmd.Context())
			if err != nil {
				return err
			}
			data, err := snap.Marshal()
			if err != nil {
				return err
			}
			if file == "" || file == "-" {
				_, err = g.out.Write(data)
				return err
			}
			if err := os.WriteFile(file, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s: %d nodes, %d slices, %d claims\n", file, len(snap.Nodes), len(snap.ResourceSlices), len(snap.ResourceClaims))
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "-", "file to write, or - for stdout")
	return cmd
}
