package VdsTest

import (
	"strings"
	"testing"

	"DataService/Channel"
	"DataService/Config"
)

func TestVdsConfigNormalize(t *testing.T) {
	dir := Channel.Config{Type: Channel.TypeDir, Path: "/tmp/x"}
	badFolder := dir
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
		{"slash in name", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a/b", Channel: dir}}}, "invalid shared vDS name"},
		{"duplicate shared", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", Channel: dir}, {Name: "a", Channel: dir}}}, "duplicate shared"},
		{"alias collides", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{
			{Name: "a", Channel: dir},
			{Name: "b", Alias: "a", Channel: dir},
		}}, "duplicate mirrored"},
		{"unknown channel", Config.VdsConfig{Mirrored: []Config.MirroredVdsConfig{{Name: "a", Channel: Channel.Config{Type: "ftp"}}}}, "ftp"},
		{"folder escapes", Config.VdsConfig{Shared: []Config.SharedVdsConfig{{Name: "a", Channel: badFolder}}}, "invalid channel folder"},
	}
	for _, c := range cases {
		err := c.cfg.Normalize()
		if err == nil || !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: expect error containing [%s], got %v", c.name, c.expect, err)
		}
	}
	// 不需要密钥；旧版本的签名 / 加密字段仍能解析，但不再要求，也不影响校验
	old := dir
	old.Encrypt = map[string]interface{}{"recipientsFile": "r"}
	ok := Config.VdsConfig{
		Shared:   []Config.SharedVdsConfig{{Name: "a", Channel: dir}, {Name: "b", SigningKeyFile: "k", Channel: old}},
		Mirrored: []Config.MirroredVdsConfig{{Name: "a", Channel: dir}},
	}
	if err := ok.Normalize(); err != nil {
		t.Fatalf("shared and mirrored may share a name and need no keys, got %v", err)
	}
	if ok.Local != nil {
		t.Fatalf("local must not be enabled implicitly when other vDS are configured")
	}
	if got := ok.Shared[0].Ignored(); len(got) != 0 {
		t.Fatalf("expect nothing ignored without old fields, got %v", got)
	}
	if got := ok.Shared[1].Ignored(); len(got) != 2 || got[0] != "signingKeyFile" || got[1] != "channel.encrypt" {
		t.Fatalf("expect the old fields reported as ignored, got %v", got)
	}
}
