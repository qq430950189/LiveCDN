package main

import (
	"os"
	"testing"
)

func TestLoadConfigAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv("CONTROLLER_LISTEN", ":18080")
	t.Setenv("ORIGIN_ADDR", "http://example-origin:8080")
	t.Setenv("RTMP_ORIGIN_ADDR", "example-origin:1935")
	t.Setenv("REG_TOKEN", "reg-from-env")
	t.Setenv("ADMIN_TOKEN", "admin-from-env")
	t.Setenv("CIPHER_SUITE", "aes128")
	t.Setenv("HB_TIMEOUT", "9")
	t.Setenv("STALE_NODE_TIMEOUT", "33")

	path := t.TempDir() + "/controller.yaml"
	if err := os.WriteFile(path, []byte("listen_addr: ':8080'\norigin_addr: 'http://origin:8080'\nreg_token: yaml-reg\nadmin_token: yaml-admin\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.ListenAddr != ":18080" || cfg.OriginAddr != "http://example-origin:8080" || cfg.RTMPOriginAddr != "example-origin:1935" {
		t.Fatalf("environment origin/listen overrides not applied: %+v", cfg)
	}
	if cfg.RegToken != "reg-from-env" || cfg.AdminToken != "admin-from-env" {
		t.Fatalf("environment token overrides not applied: %+v", cfg)
	}
	if cfg.CipherSuite != "aes128" || cfg.HBTimeout != 9 || cfg.StaleNodeTimeout != 33 {
		t.Fatalf("environment scalar overrides not applied: %+v", cfg)
	}
}
