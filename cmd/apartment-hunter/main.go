package main

import (
	"fmt"
	"os"

	"apartment-hunter/internal/adapters/cli"
)

func main() {
	if err := cli.New().Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
