package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/vood/threadpilot"
)

func main() {
	if err := threadpilot.Run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
