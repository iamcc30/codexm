package main

import (
	"os"

	"codexm/internal/cli"
)

var version = "dev"

func main() {
	app := cli.New(version)
	os.Exit(app.Run(os.Args[1:]))
}
