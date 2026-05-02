package main

import (
	"flag"
	"log"

	"github.com/user/live-cdn/internal/agent"
)

func main() {
	configPath := flag.String("config", "configs/agent.yaml", "config file path")
	flag.Parse()

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	a := agent.NewAgent(cfg)
	if err := a.Run(); err != nil {
		log.Fatalf("Agent error: %v", err)
	}
}
