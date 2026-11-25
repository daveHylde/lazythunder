// lazythunder: a lazygit-style terminal UI for Thunderbird, built on
// thunderbird-cli (`tb`) with a local llama.cpp mail assistant.
package main

import (
	"fmt"
	"os"

	"github.com/daveHylde/lazythunder/internal/agent"
	"github.com/daveHylde/lazythunder/internal/tb"
	"github.com/daveHylde/lazythunder/internal/ui"
)

func main() {
	c := tb.New()
	if err := ui.Run(c, agent.New(c)); err != nil {
		fmt.Fprintln(os.Stderr, "lazythunder:", err)
		os.Exit(1)
	}
}
