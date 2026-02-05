package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMintKeySuccess(t *testing.T) {
	// Mock OAuth token endpoint
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		r.ParseForm()
		if r.Form.Get("client_id") != "test-id" {
			t.Errorf("unexpected client_id: %s", r.Form.Get("client_id"))
		}
		if r.Form.Get("client_secret") != "test-secret" {
			t.Errorf("unexpected client_secret: %s", r.Form.Get("client_secret"))
		}
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "test-token"})
	}))
	defer tokenServer.Close()

	// Mock create key endpoint
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}

		var req createKeyRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Capabilities.Devices.Create.Ephemeral {
			t.Error("expected ephemeral=true")
		}
		if len(req.Capabilities.Devices.Create.Tags) != 1 || req.Capabilities.Devices.Create.Tags[0] != "tag:tailpod" {
			t.Errorf("unexpected tags: %v", req.Capabilities.Devices.Create.Tags)
		}

		json.NewEncoder(w).Encode(keyResponse{Key: "tskey-auth-test123"})
	}))
	defer keyServer.Close()

	// Override URLs
	origTokenURL := oauthTokenURL
	origKeyURL := createKeyURL
	oauthTokenURL = tokenServer.URL
	createKeyURL = func(tailnet string) string { return keyServer.URL }
	defer func() {
		oauthTokenURL = origTokenURL
		createKeyURL = origKeyURL
	}()

	cfg := config{
		ClientID:      "test-id",
		ClientSecret:  "test-secret",
		Tailnet:       "-",
		Expiry:        3600,
		Ephemeral:     true,
		Preauthorized: true,
	}

	key, err := mintKey(cfg, "tag:tailpod", "nginx-demo")
	if err != nil {
		t.Fatalf("mintKey error: %v", err)
	}
	if key != "tskey-auth-test123" {
		t.Errorf("unexpected key: %s", key)
	}
}

func TestMintKeyOAuthError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer tokenServer.Close()

	origTokenURL := oauthTokenURL
	oauthTokenURL = tokenServer.URL
	defer func() { oauthTokenURL = origTokenURL }()

	cfg := config{
		ClientID:     "bad-id",
		ClientSecret: "bad-secret",
		Tailnet:      "-",
	}

	_, err := mintKey(cfg, "tag:tailpod", "test")
	if err == nil {
		t.Fatal("expected error for OAuth failure")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400: %v", err)
	}
}

func TestMintKeyAPIError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{AccessToken: "test-token"})
	}))
	defer tokenServer.Close()

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"tag not allowed"}`))
	}))
	defer keyServer.Close()

	origTokenURL := oauthTokenURL
	origKeyURL := createKeyURL
	oauthTokenURL = tokenServer.URL
	createKeyURL = func(tailnet string) string { return keyServer.URL }
	defer func() {
		oauthTokenURL = origTokenURL
		createKeyURL = origKeyURL
	}()

	cfg := config{
		ClientID:     "test-id",
		ClientSecret: "test-secret",
		Tailnet:      "-",
	}

	_, err := mintKey(cfg, "tag:wrong", "test")
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention 400: %v", err)
	}
}

func TestWriteOutput(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "sub", "test.env")

	err := writeOutput(outPath, "tskey-test123", "nginx-demo")
	if err != nil {
		t.Fatalf("writeOutput error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "TS_AUTHKEY=tskey-test123") {
		t.Error("output should contain TS_AUTHKEY")
	}
	if !strings.Contains(content, "TS_HOSTNAME=nginx-demo") {
		t.Error("output should contain TS_HOSTNAME")
	}

	info, _ := os.Stat(outPath)
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected mode 0600, got %o", info.Mode().Perm())
	}
}

func TestWriteOutputNoHostname(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.env")

	err := writeOutput(outPath, "tskey-test123", "")
	if err != nil {
		t.Fatalf("writeOutput error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "TS_AUTHKEY=tskey-test123") {
		t.Error("output should contain TS_AUTHKEY")
	}
	if strings.Contains(content, "TS_HOSTNAME") {
		t.Error("output should not contain TS_HOSTNAME when not provided")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "oauth.env")
	os.WriteFile(cfgPath, []byte(`TS_API_CLIENT_ID=myid
TS_API_CLIENT_SECRET=mysecret
TS_TAILNET=example.com
TS_KEY_EXPIRY_SECONDS=7200
`), 0600)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if cfg.ClientID != "myid" {
		t.Errorf("unexpected ClientID: %s", cfg.ClientID)
	}
	if cfg.ClientSecret != "mysecret" {
		t.Errorf("unexpected ClientSecret: %s", cfg.ClientSecret)
	}
	if cfg.Tailnet != "example.com" {
		t.Errorf("unexpected Tailnet: %s", cfg.Tailnet)
	}
	if cfg.Expiry != 7200 {
		t.Errorf("unexpected Expiry: %d", cfg.Expiry)
	}
}

func TestLoadConfigMissingID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "oauth.env")
	os.WriteFile(cfgPath, []byte("TS_API_CLIENT_SECRET=secret\n"), 0600)

	_, err := loadConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing client ID")
	}
}
