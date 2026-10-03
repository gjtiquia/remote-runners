package main

import (
	"github.com/gjtiquia/remote-runners/internal/runner"
	"os"
)

func main() { os.Exit(runner.Main(os.Args[1:])) }
