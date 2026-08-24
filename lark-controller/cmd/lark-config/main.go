package main

import (
	"context"
	"fmt"
	"os"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/configcli"
)

func main() {
	if err := configcli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
