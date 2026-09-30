package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// readDotEnv parses data only: no shell execution, expansion or global env writes.
func readDotEnv(path string) (map[string]string, error) {
	values := map[string]string{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .env: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "export ") || strings.HasPrefix(text, "export\t") {
			text = strings.TrimSpace(text[7:])
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		bad := func() (map[string]string, error) { return nil, fmt.Errorf("invalid .env syntax at line %d", line) }
		if !ok || !envKeyPattern.MatchString(key) {
			return bad()
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			quote, end := value[0], -1
			for i := 1; i < len(value); i++ {
				if quote == '"' && value[i] == '\\' {
					i++
					continue
				}
				if value[i] == quote {
					end = i
					break
				}
			}
			if end < 0 {
				return bad()
			}
			tail := strings.TrimSpace(value[end+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return bad()
			}
			if quote == '"' {
				decoded, err := strconv.Unquote(value[:end+1])
				if err != nil {
					return bad()
				}
				value = decoded
			} else {
				value = value[1:end]
			}
		} else {
			for i := 0; i < len(value); i++ {
				if value[i] == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
					value = strings.TrimSpace(value[:i])
					break
				}
			}
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("cannot read .env near line %d", line+1)
	}
	return values, nil
}
