package runengine

import (
	"strings"
	"testing"
)

func TestSealSecretsRoundTrip(t *testing.T) {
	t.Setenv("TAKO_JWT_SECRET", "dev-only-jwt-secret-change-me-32bytes")
	t.Setenv("TAKO_DATA_KEY", "")
	sealed, err := sealSecrets(map[string]string{"apiKey": "sk-live-plain"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := sealed["apiKey"]
	if strings.Contains(stored, "sk-live-plain") || !strings.HasPrefix(stored, "seal1:") {
		t.Fatalf("落库值应是 seal1 密文，得到 %q", stored)
	}
	plain, err := unsealSecret(stored)
	if err != nil || plain != "sk-live-plain" {
		t.Fatalf("解开应回到原文，得到 %q %v", plain, err)
	}
	kept, err := sealSecrets(map[string]string{"apiKey": "••••"}, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if kept["apiKey"] != stored {
		t.Fatalf("掩码回写应保住原密文，得到 %q", kept["apiKey"])
	}
	omitted, err := sealSecrets(nil, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if omitted["apiKey"] != stored {
		t.Fatalf("没带 secrets 应沿用原密文，得到 %q", omitted["apiKey"])
	}
}

func TestSealKeyRotation(t *testing.T) {
	const jwt = "dev-only-jwt-secret-change-me-32bytes"
	const jwtNext = "rotated-jwt-secret-value-32bytes!!"
	const dataA = "data-key-aaaaaaaaaaaaaaaaaaaaaaaa32"
	const dataB = "data-key-bbbbbbbbbbbbbbbbbbbbbbbb32"
	t.Setenv("TAKO_JWT_SECRET", jwt)
	t.Setenv("TAKO_JWT_SECRET_PREVIOUS", "")
	t.Setenv("TAKO_DATA_KEY", "")
	t.Setenv("TAKO_DATA_KEY_PREVIOUS", "")
	old, err := sealSecret("sk-old")
	if err != nil || !strings.HasPrefix(old, "seal1:") {
		t.Fatalf("没配数据密钥时应写成 seal1，得到 %q %v", old, err)
	}
	t.Setenv("TAKO_JWT_SECRET", jwtNext)
	if _, err := unsealSecret(old); err == nil {
		t.Fatal("换掉登录密钥后，旧 seal1 不应被新密钥解开")
	}
	t.Setenv("TAKO_JWT_SECRET_PREVIOUS", jwt)
	if plain, err := unsealSecret(old); err != nil || plain != "sk-old" {
		t.Fatalf("上一把登录密钥应解开旧 seal1，得到 %q %v", plain, err)
	}
	t.Setenv("TAKO_DATA_KEY", dataA)
	fresh, err := sealSecret("sk-new")
	if err != nil || !strings.HasPrefix(fresh, "seal2:") {
		t.Fatalf("配了数据密钥时应写成 seal2，得到 %q %v", fresh, err)
	}
	if plain, err := unsealSecret(old); err != nil || plain != "sk-old" {
		t.Fatalf("数据密钥不应挡住旧 seal1，得到 %q %v", plain, err)
	}
	t.Setenv("TAKO_DATA_KEY", dataB)
	if _, err := unsealSecret(fresh); err == nil {
		t.Fatal("换掉数据密钥后，旧 seal2 不应被新密钥解开")
	}
	t.Setenv("TAKO_DATA_KEY_PREVIOUS", dataA)
	if plain, err := unsealSecret(fresh); err != nil || plain != "sk-new" {
		t.Fatalf("上一把数据密钥应解开旧 seal2，得到 %q %v", plain, err)
	}
}
