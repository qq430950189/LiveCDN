package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

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
		cfg := &controller.Config{
			RegToken:   "change-me-reg-token",
			AdminToken: "change-me-admin-token",
		}
		controller.ApplyConfigDefaults(cfg)
		applyEnvOverrides(cfg)
		return cfg, nil
	}

	cfg := &controller.Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	controller.ApplyConfigDefaults(cfg)
	applyEnvOverrides(cfg)
	return cfg, nil
}

func applyEnvOverrides(cfg *controller.Config) {
	if v := os.Getenv("CONTROLLER_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("ORIGIN_ADDR"); v != "" {
		cfg.OriginAddr = v
	}
	if v := os.Getenv("RTMP_ORIGIN_ADDR"); v != "" {
		cfg.RTMPOriginAddr = v
	}
	if v := os.Getenv("REG_TOKEN"); v != "" {
		cfg.RegToken = v
	}
	if v := os.Getenv("ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("CIPHER_SUITE"); v != "" {
		cfg.CipherSuite = v
	}
	if v := os.Getenv("HB_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.HBTimeout = n
		}
	}
	if v := os.Getenv("STALE_NODE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.StaleNodeTimeout = n
		}
	}
	if v := os.Getenv("BINARY_DIR"); v != "" {
		cfg.BinaryDir = v
	}
	if v := os.Getenv("INSTALL_SCRIPT_PATH"); v != "" {
		cfg.InstallScriptPath = v
	}
}
