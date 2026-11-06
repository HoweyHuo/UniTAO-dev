package VdsTest

import (
	"strings"
	"testing"

	"DataService/Channel"
	"DataService/Config"
)

func TestVdsConfigNormalize(t *testing.T) {
	dir := Channel.Config{Type: Channel.TypeDir, Path: "/tmp/x"}
	empty := Config.VdsConfig{}
	if err := empty.Normalize(); err != nil || empty.Local == nil {
		t.Fatalf("expect empty vds section to default to local, err=%v", err)
	}
	cases := []struct {
		name   string
		cfg    Config.VdsConfig
		expect string
	}{
		{"slash in name", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a/b", SigningKeyFile: "k", Channel: dir}}}, "invalid shared vDS name"},
		{"duplicate shared", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: dir}, {Name: "a", SigningKeyFile: "k", Channel: dir}}}, "duplicate shared"},
		{"shared without key", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", Channel: dir}}}, "signingKeyFile"},
		{"mirrored without fingerprint", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", Channel: dir}}}, "trustedFingerprint"},
		{"alias collides", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{
			{Name: "a", TrustedFingerprint: "f1", Channel: dir},
			{Name: "b", Alias: "a", TrustedFingerprint: "f2", Channel: dir},
		}}, "duplicate mirrored"},
		{"unknown channel", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", TrustedFingerprint: "f", Channel: Channel.Config{Type: "ftp"}}}}, "ftp"},
	}
	for _, c := range cases {
		err := c.cfg.Normalize()
		if err == nil || !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: expect error containing [%s], got %v", c.name, c.expect, err)
		}
	}
	ok := Config.VdsConfig{
		Shared:   []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: dir}},
		Mirrored: []Config.MirroredVdsConfig{{Name: "a", TrustedFingerprint: "f", Channel: dir}},
	}
	if err := ok.Normalize(); err != nil {
		t.Fatalf("shared and mirrored may share a name, got %v", err)
	}
	if ok.Local != nil {
		t.Fatalf("local must not be enabled implicitly when other vDS are configured")
	}
}
