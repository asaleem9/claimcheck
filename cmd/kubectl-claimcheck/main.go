// Command kubectl-claimcheck explains why Kubernetes can't allocate DRA
// devices to a pod. Installed on PATH, it runs as `kubectl claimcheck`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/asaleem9/claimcheck/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.NewRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
