package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type config struct {
	ClientID     string
	ClientSecret string
	Tailnet      string
	Expiry       int
	Ephemeral    bool
	Reusable     bool
	Preauthorized bool
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

type createKeyRequest struct {
	Capabilities capabilities `json:"capabilities"`
	ExpirySeconds int         `json:"expirySeconds"`
	Description   string      `json:"description,omitempty"`
}

type capabilities struct {
	Devices deviceCaps `json:"devices"`
}

type deviceCaps struct {
	Create createCaps `json:"create"`
}

type createCaps struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags"`
}

type keyResponse struct {
	Key string `json:"key"`
}

// Overridable for testing
var (
	oauthTokenURL = "https://api.tailscale.com/api/v2/oauth/token"
	createKeyURL  = func(tailnet string) string {
		return fmt.Sprintf("https://api.tailscale.com/api/v2/tailnet/%s/keys", tailnet)
	}
	httpClient = &http.Client{}
)

func main() {
	configFile := flag.String("config", "/etc/tailscale/oauth.env", "Path to OAuth config env file")
	tag := flag.String("tag", "", "Tag to apply (e.g. tag:tailpod)")
	output := flag.String("output", "", "Output file for TS_AUTHKEY=... env file")
	hostname := flag.String("hostname", "", "Hostname to include as TS_HOSTNAME=...")
	flag.Parse()

	if *tag == "" {
		fmt.Fprintln(os.Stderr, "error: -tag is required")
		os.Exit(2)
	}
	if *output == "" {
		fmt.Fprintln(os.Stderr, "error: -output is required")
		os.Exit(2)
	}

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	authKey, err := mintKey(cfg, *tag, *hostname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := writeOutput(*output, authKey, *hostname); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	env := parseEnvFile(string(data))
	c := config{
		ClientID:      env["TS_API_CLIENT_ID"],
		ClientSecret:  env["TS_API_CLIENT_SECRET"],
		Tailnet:       env["TS_TAILNET"],
		Expiry:        3600,
		Ephemeral:     true,
		Reusable:      false,
		Preauthorized: true,
	}

	if c.ClientID == "" {
		return config{}, fmt.Errorf("TS_API_CLIENT_ID not set in %s", path)
	}
	if c.ClientSecret == "" {
		return config{}, fmt.Errorf("TS_API_CLIENT_SECRET not set in %s", path)
	}
	if c.Tailnet == "" {
		c.Tailnet = "-"
	}
	if v, ok := env["TS_KEY_EXPIRY_SECONDS"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.Expiry = n
		}
	}
	if v, ok := env["TS_KEY_EPHEMERAL"]; ok {
		c.Ephemeral = v == "true"
	}
	if v, ok := env["TS_KEY_REUSABLE"]; ok {
		c.Reusable = v == "true"
	}
	if v, ok := env["TS_KEY_PREAUTHORIZED"]; ok {
		c.Preauthorized = v == "true"
	}

	return c, nil
}

func parseEnvFile(data string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx >= 0 {
			env[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
		}
	}
	return env
}

func mintKey(cfg config, tag, hostname string) (string, error) {
	// Step 1: Get OAuth access token
	accessToken, err := getAccessToken(cfg)
	if err != nil {
		return "", fmt.Errorf("getting access token: %w", err)
	}

	// Step 2: Create auth key
	desc := "minted-by-quadlet-deploy"
	if hostname != "" {
		desc = hostname
	}

	reqBody := createKeyRequest{
		Capabilities: capabilities{
			Devices: deviceCaps{
				Create: createCaps{
					Reusable:      cfg.Reusable,
					Ephemeral:     cfg.Ephemeral,
					Preauthorized: cfg.Preauthorized,
					Tags:          []string{tag},
				},
			},
		},
		ExpirySeconds: cfg.Expiry,
		Description:   desc,
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", createKeyURL(cfg.Tailnet), strings.NewReader(string(bodyJSON)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("creating auth key: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create key API returned %d: %s", resp.StatusCode, body)
	}

	var keyResp keyResponse
	if err := json.Unmarshal(body, &keyResp); err != nil {
		return "", fmt.Errorf("parsing key response: %w", err)
	}
	if keyResp.Key == "" {
		return "", fmt.Errorf("empty key in response: %s", body)
	}

	return keyResp.Key, nil
}

func getAccessToken(cfg config) (string, error) {
	data := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}

	resp, err := httpClient.PostForm(oauthTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("OAuth token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OAuth token API returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("empty access_token in response: %s", body)
	}

	return tokenResp.AccessToken, nil
}

func writeOutput(path, authKey, hostname string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	content := fmt.Sprintf("TS_AUTHKEY=%s\n", authKey)
	if hostname != "" {
		content += fmt.Sprintf("TS_HOSTNAME=%s\n", hostname)
	}

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Chown to SUDO_UID/SUDO_GID if running under sudo
	chownIfSudo(tmpPath)

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

func chownIfSudo(path string) {
	uidStr := os.Getenv("SUDO_UID")
	gidStr := os.Getenv("SUDO_GID")
	if uidStr == "" || gidStr == "" {
		return
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(gidStr)
	if err != nil {
		return
	}
	os.Chown(path, uid, gid)
}
