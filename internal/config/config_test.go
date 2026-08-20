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

// TestSkillsEnabledDefaultsOn pins the default-on rule from every direction a
// config file can express it. Getting this backwards would silently withdraw a
// capability from every deployment that never mentioned skills.
func TestSkillsEnabledDefaultsOn(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"section omitted", "server:\n  mode: stdio\n", true},
		{"section empty", "server:\n  mode: stdio\nskills: {}\n", true},
		{"enabled omitted", "server:\n  mode: stdio\nskills:\n  {}\n", true},
		{"explicitly true", "server:\n  mode: stdio\nskills:\n  enabled: true\n", true},
		{"explicitly false", "server:\n  mode: stdio\nskills:\n  enabled: false\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("LoadFromBytes: %v", err)
			}
			if got := cfg.SkillsEnabled(); got != tc.want {
				t.Errorf("SkillsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDefaultServesSkills covers the zero-config launch, which never sees a
// skills: section at all.
func TestDefaultServesSkills(t *testing.T) {
	dir := t.TempDir()
	writeKubeconfig(t, dir, ".kube/config")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", dir)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if !cfg.SkillsEnabled() {
		t.Error("a zero-config launch should serve skills")
	}
}
