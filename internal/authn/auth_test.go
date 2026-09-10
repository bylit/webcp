package authn

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("AUTH_USER", "alice")
	t.Setenv("AUTH_PASSWORD", "correct horse battery staple")
	t.Setenv("AUTH_CREDENTIALS_FILE", "")

	config, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.Username != "alice" || config.Password != "correct horse battery staple" {
		t.Fatalf("unexpected authentication config: %#v", config)
	}
}

func TestCredentialsFileOverridesEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("file-user:file:password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("AUTH_USER", "environment-user")
	t.Setenv("AUTH_PASSWORD", "environment-password")
	t.Setenv("AUTH_CREDENTIALS_FILE", path)

	config, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.Username != "file-user" || config.Password != "file:password" {
		t.Fatalf("file credentials were not loaded: %#v", config)
	}
}

func TestCredentialsFileKeyValueAndJSONFormats(t *testing.T) {
	tests := []struct {
		name    string
		content string
		user    string
		pass    string
	}{
		{name: "key value", content: "username=operator\npassword=secret value\n", user: "operator", pass: "secret value"},
		{name: "json", content: `{"username":"json-user","password":"json-pass"}`, user: "json-user", pass: "json-pass"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			user, password, err := readCredentialsFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if user != test.user || password != test.pass {
				t.Fatalf("got %q/%q, want %q/%q", user, password, test.user, test.pass)
			}
		})
	}
}

func TestEnabledAuthenticationRequiresCompleteCredentials(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("AUTH_USER", "alice")
	t.Setenv("AUTH_PASSWORD", "")
	t.Setenv("AUTH_CREDENTIALS_FILE", "")
	if _, err := LoadFromEnv(); err == nil {
		t.Fatal("expected incomplete authentication configuration to fail")
	}
}

func TestProtect(t *testing.T) {
	protected := Config{Enabled: true, Username: "alice", Password: "secret"}.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	unauthorized := httptest.NewRecorder()
	protected.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/downloads", nil))
	if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unexpected unauthorized response: %d %#v", unauthorized.Code, unauthorized.Header())
	}

	wrongPassword := httptest.NewRequest(http.MethodGet, "/", nil)
	wrongPassword.SetBasicAuth("alice", "wrong")
	wrong := httptest.NewRecorder()
	protected.ServeHTTP(wrong, wrongPassword)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password returned %d", wrong.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/downloads", nil)
	authorizedRequest.SetBasicAuth("alice", "secret")
	authorized := httptest.NewRecorder()
	protected.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("authorized request returned %d", authorized.Code)
	}

	health := httptest.NewRecorder()
	protected.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("health check should be public, got %d", health.Code)
	}
}
