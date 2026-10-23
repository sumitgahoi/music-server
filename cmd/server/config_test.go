package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRootFlag(t *testing.T) {
	root, err := resolveRoot("", "/srv/music")
	if err != nil {
		t.Fatal(err)
	}
	if root != "/srv/music" {
		t.Fatalf("root = %q", root)
	}
	if _, err := resolveRoot("", ""); err != errUsage {
		t.Fatalf("missing folder: %v", err)
	}
	if _, err := resolveRoot("/etc/music-server.conf", "/srv/music"); err == nil {
		t.Fatal("config and root were both accepted")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "music-server.conf")
	body := "# site\n\nmusic_root = /srv/my music\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MusicRoot != "/srv/my music" {
		t.Fatalf("root = %q", cfg.MusicRoot)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := []string{
		"",
		"music_root=\n",
		"music_root=/a\nmusic_root=/b\n",
		"port=80\nmusic_root=/a\n",
		"music_root\n",
	}
	for _, body := range cases {
		_, err := parseConfig("music-server.conf", strings.NewReader(body))
		if err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
