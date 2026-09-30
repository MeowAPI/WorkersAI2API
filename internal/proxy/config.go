package proxy

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config contains server-side credentials; client credentials are never forwarded.
type Config struct {
	Address      string
	AuthToken    string
	AccountsFile string
	LogsDir      string
}

func LoadConfig() (Config, error) {
	values, err := readDotEnv(".env")
	if err != nil {
		return Config{}, err
	}
	get := func(key, fallback string) string {
		value, exists := os.LookupEnv(key)
		if !exists {
			value = values[key]
		}
		if value == "" {
			return fallback
		}
		return value
	}
	c := Config{AuthToken: get("AUTH_TOKEN", ""), AccountsFile: "data/accounts.json", LogsDir: "data/logs"}
	port := get("PORT", "8080")
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("PORT must be between 1 and 65535")
	}
	c.Address = net.JoinHostPort(get("BIND_ADDRESS", "0.0.0.0"), port)
	return c, c.Validate()
}

var accountIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

func validSecret(value string) bool { return value != "" && !strings.ContainsAny(value, " \t\r\n") }
func (c Config) Validate() error {
	if !validSecret(c.AuthToken) {
		return fmt.Errorf("AUTH_TOKEN is required and must not contain whitespace")
	}
	return nil
}
