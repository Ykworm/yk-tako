package runengine

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
)

// seal1 用 TAKO_JWT_SECRET 派生。seal2 用独立的 TAKO_DATA_KEY。
// 新写入在配了数据密钥时走 seal2，这样轮换登录密钥不会把旧的租户密钥解不开。
// 解开时先试当前密钥，再试 *_PREVIOUS，旧密文在轮换后的进程里仍能打开。
const (
	seal1Prefix = "seal1:"
	seal2Prefix = "seal2:"
)

func isSealedValue(v string) bool {
	return strings.HasPrefix(v, seal1Prefix) || strings.HasPrefix(v, seal2Prefix)
}

func keyFromEnv(name string) ([]byte, bool) {
	raw := os.Getenv(name)
	if len(raw) < 32 {
		return nil, false
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], true
}

func encryptSecret(prefix string, key []byte, plain string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func decryptSecret(prefix string, key []byte, stored string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed secret too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func unsealWith(stored, prefix string, names ...string) (string, error) {
	var last error
	for _, name := range names {
		key, ok := keyFromEnv(name)
		if !ok {
			continue
		}
		plain, err := decryptSecret(prefix, key, stored)
		if err == nil {
			return plain, nil
		}
		last = err
	}
	if last == nil {
		return "", errors.New("没有能解开这份 secret 的密钥")
	}
	return "", last
}

func sealSecret(plain string) (string, error) {
	if plain == "" || isSealedValue(plain) {
		return plain, nil
	}
	if key, ok := keyFromEnv("TAKO_DATA_KEY"); ok {
		return encryptSecret(seal2Prefix, key, plain)
	}
	key, ok := keyFromEnv("TAKO_JWT_SECRET")
	if !ok {
		return "", errors.New("TAKO_JWT_SECRET 太短，不能封存 runtime secret")
	}
	return encryptSecret(seal1Prefix, key, plain)
}

func unsealSecret(stored string) (string, error) {
	switch {
	case stored == "":
		return "", nil
	case strings.HasPrefix(stored, seal2Prefix):
		return unsealWith(stored, seal2Prefix, "TAKO_DATA_KEY", "TAKO_DATA_KEY_PREVIOUS")
	case strings.HasPrefix(stored, seal1Prefix):
		return unsealWith(stored, seal1Prefix, "TAKO_JWT_SECRET", "TAKO_JWT_SECRET_PREVIOUS")
	default:
		return stored, nil
	}
}

// sealSecrets 把即将落库的 secret 封存。next == nil 表示客户端没带 secrets，沿用 prev。
// 值是掩码「••••」时也沿用 prev 里同一键，避免把 GET 的响应写回去盖掉真值。
func sealSecrets(next, prev map[string]string) (map[string]string, error) {
	if next == nil {
		next = prev
	}
	if len(next) == 0 {
		return next, nil
	}
	out := make(map[string]string, len(next))
	for k, v := range next {
		if v == "••••" {
			if prev != nil {
				v = prev[k]
			} else {
				v = ""
			}
		}
		sealed, err := sealSecret(v)
		if err != nil {
			return nil, err
		}
		out[k] = sealed
	}
	return out, nil
}

func secretsNeedSeal(m map[string]string) bool {
	for _, v := range m {
		if v != "" && !isSealedValue(v) {
			return true
		}
	}
	return false
}
