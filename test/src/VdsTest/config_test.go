package VdsTest

import (
	"strings"
	"testing"

	"DataService/Channel"
	"DataService/Config"
)

func TestVdsConfigNormalize(t *testing.T) {
	dir := Channel.Config{Type: Channel.TypeDir, Path: "/tmp/x"}
	pubDir := dir
	pubDir.Encrypt = &Channel.EncryptConfig{RecipientsFile: "r"}
	subDir := dir
	subDir.Decrypt = &Channel.DecryptConfig{IdentityFile: "i"}
	badFolder := pubDir
	badFolder.Folder = "../escape"
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
		{"duplicate shared", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: pubDir}, {Name: "a", SigningKeyFile: "k", Channel: pubDir}}}, "duplicate shared"},
		{"shared without key", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", Channel: dir}}}, "signingKeyFile"},
		{"mirrored without fingerprint", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", Channel: dir}}}, "trustedFingerprint"},
		{"alias collides", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{
			{Name: "a", TrustedFingerprint: "f1", Channel: subDir},
			{Name: "b", Alias: "a", TrustedFingerprint: "f2", Channel: subDir},
		}}, "duplicate mirrored"},
		{"unknown channel", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", TrustedFingerprint: "f", Channel: Channel.Config{Type: "ftp"}}}}, "ftp"},
		{"shared without recipients", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: dir}}}, "recipientsFile"},
		{"mirrored without identity", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", TrustedFingerprint: "f", Channel: dir}}}, "identityFile"},
		{"folder escapes", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: badFolder}}}, "invalid channel folder"},
	}
	for _, c := range cases {
		err := c.cfg.Normalize()
		if err == nil || !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: expect error containing [%s], got %v", c.name, c.expect, err)
		}
	}
	ok := Config.VdsConfig{
		Shared:   []Config.SharedVdsConfig{{Name: "a", SigningKeyFile: "k", Channel: pubDir}},
		Mirrored: []Config.MirroredVdsConfig{{Name: "a", TrustedFingerprint: "f", Channel: subDir}},
	}
	if err := ok.Normalize(); err != nil {
		t.Fatalf("shared and mirrored may share a name, got %v", err)
	}
	if ok.Local != nil {
		t.Fatalf("local must not be enabled implicitly when other vDS are configured")
	}
}
