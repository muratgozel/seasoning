package paths

import (
	"path/filepath"
	"testing"
)

func TestDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{EnvConfigDir, EnvDataDir, EnvStateDir, EnvBinDir, EnvVersionsDir, "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(v, "")
	}
	check := func(name string, f func() (string, error), want string) {
		t.Helper()
		got, err := f()
		if err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	check("config", ConfigDir, filepath.Join(home, ".config", "seasoning"))
	check("data", DataDir, filepath.Join(home, ".local", "share", "seasoning"))
	check("state", StateDir, filepath.Join(home, ".local", "state", "seasoning"))
	check("versions", VersionsDir, filepath.Join(home, ".local", "share", "seasoning", "versions"))
	check("bin", BinDir, filepath.Join(home, ".local", "bin"))

	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_DATA_HOME", "relative/ignored")
	check("config xdg", ConfigDir, filepath.Join("/xdg/config", "seasoning"))
	check("data relative xdg", DataDir, filepath.Join(home, ".local", "share", "seasoning"))

	t.Setenv(EnvConfigDir, "/override/config")
	t.Setenv(EnvVersionsDir, "/override/versions")
	check("config override", ConfigDir, "/override/config")
	check("versions override", VersionsDir, "/override/versions")
}
