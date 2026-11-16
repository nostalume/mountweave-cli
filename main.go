package main

import (
	"fmt"
	"os"

	"github.com/nostalume/mountweave-cli/cli"
)

func main() {
	app, err := cli.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(app.Main())
}
