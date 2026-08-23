package main

import (
	"context"
	"fmt"
	"os"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/correctioncli"
)

func main() {
	if err := correctioncli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, nil); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
