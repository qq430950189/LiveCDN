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
			ListenAddr:        ":8080",
			OriginAddr:        "http://origin:8080",
			RTMPOriginAddr:    "origin:1935",
			RegToken:          "change-me-reg-token",
			AdminToken:        "change-me-admin-token",
			CipherSuite:       "chacha20-poly1305",
			HBTimeout:         15,
			StaleNodeTimeout:  120,
			BinaryDir:         "./binaries",
			InstallScriptPath: "./deploy/install.sh",
		}
		applyEnvOverrides(cfg)
		return cfg, nil
	}

	cfg := &controller.Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.OriginAddr == "" {
		cfg.OriginAddr = "http://origin:8080"
	}
	if cfg.RTMPOriginAddr == "" {
		cfg.RTMPOriginAddr = "origin:1935"
	}
	if cfg.CipherSuite == "" {
		cfg.CipherSuite = "chacha20-poly1305"
	}
	if cfg.HBTimeout == 0 {
		cfg.HBTimeout = 15
	}
	if cfg.StaleNodeTimeout == 0 {
		cfg.StaleNodeTimeout = 120
	}
	if cfg.BinaryDir == "" {
		cfg.BinaryDir = "./binaries"
	}
	if cfg.InstallScriptPath == "" {
		cfg.InstallScriptPath = "./deploy/install.sh"
	}

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
