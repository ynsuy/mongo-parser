// Command mongo-log-parser reads a MongoDB support bundle and answers questions about it.
package main

import (
	"os"

	"mongo-log-parser/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
