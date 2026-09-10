package authn

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const maxCredentialsFileSize = 64 * 1024

type Config struct {
	Enabled  bool
	Username string
	Password string
}

func LoadFromEnv() (Config, error) {
	enabledValue := strings.TrimSpace(os.Getenv("AUTH_ENABLED"))
	if enabledValue == "" {
		return Config{}, nil
	}
	enabled, err := strconv.ParseBool(enabledValue)
	if err != nil {
		return Config{}, fmt.Errorf("AUTH_ENABLED must be true or false: %w", err)
	}
	if !enabled {
		return Config{}, nil
	}

	config := Config{
		Enabled:  true,
		Username: os.Getenv("AUTH_USER"),
		Password: os.Getenv("AUTH_PASSWORD"),
	}
	if path := strings.TrimSpace(os.Getenv("AUTH_CREDENTIALS_FILE")); path != "" {
		username, password, err := readCredentialsFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("load AUTH_CREDENTIALS_FILE: %w", err)
		}
		config.Username = username
		config.Password = password
	}
	if config.Username == "" {
		return Config{}, errors.New("authentication is enabled but AUTH_USER is empty")
	}
	if config.Password == "" {
		return Config{}, errors.New("authentication is enabled but AUTH_PASSWORD is empty")
	}
	return config, nil
}

func (config Config) Protect(next http.Handler) http.Handler {
	if !config.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health checks do not carry credentials and expose no application data.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		usernameMatches := constantTimeEqual(username, config.Username)
		passwordMatches := constantTimeEqual(password, config.Password)
		if !ok || usernameMatches&passwordMatches != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="webcp", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Authentication required.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func readCredentialsFile(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialsFileSize+1))
	if err != nil {
		return "", "", err
	}
	if len(data) > maxCredentialsFileSize {
		return "", "", errors.New("credentials file is too large")
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return "", "", errors.New("credentials file is empty")
	}

	if strings.HasPrefix(content, "{") {
		var values struct {
			Username string `json:"username"`
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(data, &values); err != nil {
			return "", "", fmt.Errorf("decode JSON credentials: %w", err)
		}
		if values.Username == "" {
			values.Username = values.User
		}
		return validateCredentials(values.Username, values.Password)
	}

	lines := make([]string, 0, 2)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	if len(lines) == 1 {
		parts := strings.SplitN(lines[0], ":", 2)
		if len(parts) != 2 {
			return "", "", errors.New("single-line credentials must use username:password")
		}
		return validateCredentials(strings.TrimSpace(parts[0]), parts[1])
	}

	var username, password string
	for _, line := range lines {
		separator := "="
		if !strings.Contains(line, separator) {
			separator = ":"
		}
		parts := strings.SplitN(line, separator, 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid credentials line %q", line)
		}
		switch strings.ToLower(strings.TrimSpace(parts[0])) {
		case "user", "username":
			username = strings.TrimSpace(parts[1])
		case "password":
			password = strings.TrimSpace(parts[1])
		default:
			return "", "", fmt.Errorf("unknown credentials key %q", strings.TrimSpace(parts[0]))
		}
	}
	return validateCredentials(username, password)
}

func validateCredentials(username, password string) (string, string, error) {
	if username == "" || password == "" {
		return "", "", errors.New("credentials file must contain a username and password")
	}
	return username, password, nil
}

func constantTimeEqual(actual, expected string) int {
	actualHash := sha256.Sum256([]byte(actual))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(actualHash[:], expectedHash[:])
}
