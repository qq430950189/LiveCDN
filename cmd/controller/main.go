package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/user/live-cdn/internal/controller"
	"gopkg.in/yaml.v3"
)

func main() {
	configPath := flag.String("config", "configs/controller.yaml", "config file path")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	server := controller.NewServer(cfg)
	if err := server.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func loadConfig(path string) (*controller.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Use defaults
		log.Printf("Config file not found (%s), using defaults", path)
		return &controller.Config{
			ListenAddr:       ":8080",
			OriginAddr:       "http://origin:8080",
			RegToken:         "change-me-reg-token",
			AdminToken:       "change-me-admin-token",
			CipherSuite:      "chacha20",
			HBTimeout:        15,
			StaleNodeTimeout: 120,
		}, nil
	}

	cfg := &controller.Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.CipherSuite == "" {
		cfg.CipherSuite = "chacha20"
	}
	if cfg.HBTimeout == 0 {
		cfg.HBTimeout = 15
	}
	if cfg.StaleNodeTimeout == 0 {
		cfg.StaleNodeTimeout = 120
	}

	return cfg, nil
}
