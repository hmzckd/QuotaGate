// Package envfile reads KEY=value files such as .env.compose.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// Read returns the KEY=value pairs in path. Blank lines and lines starting
// with # are skipped; one pair of surrounding quotes is removed from values.
func Read(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, scanner.Err()
}
