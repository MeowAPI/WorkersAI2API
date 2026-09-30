package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotEnvLoadsWithoutShellAndEnvironmentWins(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, key := range []string{"AUTH_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_TOKEN", "PORT", "BIND_ADDRESS"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	data := "\ufeff# local configuration\r\nexport AUTH_TOKEN = 'from-file#literal' # comment\r\nPORT=9012\nBIND_ADDRESS=127.0.0.1\nLITERAL=$(touch should-not-exist)\nESCAPED=\"first\\nsecond\"\n"
	if err := os.WriteFile(".env", []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "from-file#literal" || cfg.Address != "127.0.0.1:9012" {
		t.Fatal(cfg.Address)
	}
	if _, err := os.Stat("should-not-exist"); !os.IsNotExist(err) {
		t.Fatal("dotenv executed a command")
	}
	values, err := readDotEnv(".env")
	if err != nil || values["ESCAPED"] != "first\nsecond" {
		t.Fatal(values, err)
	}
	t.Setenv("PORT", "8089")
	t.Setenv("AUTH_TOKEN", "from-process")
	cfg, err = LoadConfig()
	if err != nil || cfg.AuthToken != "from-process" || cfg.Address != "127.0.0.1:8089" {
		t.Fatal(err, cfg.Address)
	}
	t.Setenv("AUTH_TOKEN", "")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("explicit empty environment value must override file")
	}
}
func TestDotEnvErrorsDoNotExposeSecrets(t *testing.T) {
	for _, value := range []string{"AUTH_TOKEN=\"secret-unterminated", "AUTH_TOKEN='secret' garbage", "not-a-key=secret", "AUTH_TOKEN=\"secret\\q\""} {
		file := filepath.Join(t.TempDir(), ".env")
		os.WriteFile(file, []byte(value), 0600)
		_, err := readDotEnv(file)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
	if _, err := readDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}
