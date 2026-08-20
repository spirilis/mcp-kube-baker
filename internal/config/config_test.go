package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig drops an empty placeholder file — Default only checks
// existence/readability, not content.
func writeKubeconfig(t *testing.T, dir string, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultUsesKubeconfigEnv(t *testing.T) {
	path := writeKubeconfig(t, t.TempDir(), "kc.yaml")
	t.Setenv("KUBECONFIG", path)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.Kubeconfig != path {
		t.Errorf("Kubeconfig = %q, want %q", cfg.Kubeconfig, path)
	}
	if cfg.Server.Mode != "stdio" {
		t.Errorf("Mode = %q, want stdio", cfg.Server.Mode)
	}
	if cfg.Server.LegacyCompat == nil || !cfg.Server.LegacyCompat.Enabled {
		t.Error("legacy compat should be enabled by default")
	}
	if cfg.Logging == nil || cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("unexpected logging defaults: %+v", cfg.Logging)
	}
}

func TestDefaultUsesFirstKubeconfigListEntry(t *testing.T) {
	dir := t.TempDir()
	first := writeKubeconfig(t, dir, "first.yaml")
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+filepath.Join(dir, "second.yaml"))

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.Kubeconfig != first {
		t.Errorf("Kubeconfig = %q, want first list entry %q", cfg.Kubeconfig, first)
	}
}

func TestDefaultFallsBackToHomeDotKube(t *testing.T) {
	home := t.TempDir()
	path := writeKubeconfig(t, home, filepath.Join(".kube", "config"))
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", home)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.Kubeconfig != path {
		t.Errorf("Kubeconfig = %q, want %q", cfg.Kubeconfig, path)
	}
}

func TestDefaultResolvesEvenWhenFileMissing(t *testing.T) {
	// Default only resolves the path; existence is judged later by
	// ValidateKubeconfig on the final (possibly flag-overridden) path.
	home := t.TempDir() // no .kube/config inside
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", home)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default should not fail on a missing file: %v", err)
	}
	want := filepath.Join(home, ".kube", "config")
	if cfg.Kubeconfig != want {
		t.Errorf("Kubeconfig = %q, want %q", cfg.Kubeconfig, want)
	}
	if err := ValidateKubeconfig(cfg.Kubeconfig); err == nil {
		t.Error("ValidateKubeconfig should reject the nonexistent path")
	}
}

func TestValidateKubeconfigEmpty(t *testing.T) {
	err := ValidateKubeconfig("")
	if err == nil || !strings.Contains(err.Error(), "kubeconfig is required") {
		t.Fatalf("expected kubeconfig-required error, got %v", err)
	}
}

func TestLoadFromBytesAllowsOmittedKubeconfig(t *testing.T) {
	// A config file may omit kubeconfig: — the caller falls back to
	// $KUBECONFIG / ~/.kube/config resolution.
	cfg, err := LoadFromBytes([]byte("server:\n  mode: stdio\n"))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Kubeconfig != "" {
		t.Errorf("Kubeconfig = %q, want empty", cfg.Kubeconfig)
	}
}
