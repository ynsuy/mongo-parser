// Command mongo-log-parser reads a MongoDB support bundle and answers questions about it.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"

	"mongo-log-parser/internal/cli"
)

func main() {
	watchParent()
	// MONGO_LOG_PARSER_PROFILE=<dir> writes cpu.pprof and heap.pprof for one run, for go tool pprof.
	dir := os.Getenv("MONGO_LOG_PARSER_PROFILE")
	if dir == "" {
		os.Exit(cli.Run(os.Args[1:]))
	}
	_ = os.MkdirAll(dir, 0o755)
	if f, err := os.Create(filepath.Join(dir, "cpu.pprof")); err == nil {
		_ = pprof.StartCPUProfile(f)
	}
	code := cli.Run(os.Args[1:])
	pprof.StopCPUProfile()
	if f, err := os.Create(filepath.Join(dir, "heap.pprof")); err == nil {
		runtime.GC()
		_ = pprof.Lookup("allocs").WriteTo(f, 0)
		f.Close()
	}
	os.Exit(code)
}

// watchParent exits when the MCP server that started this run is gone, even if it was killed too
// hard to kill its children first.
func watchParent() {
	parent, err := strconv.Atoi(os.Getenv("MONGO_LOG_PARSER_PARENT"))
	if err != nil || parent <= 1 {
		return
	}
	go func() {
		for range time.Tick(2 * time.Second) {
			if os.Getppid() != parent {
				os.Exit(3)
			}
		}
	}()
}
