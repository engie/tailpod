package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// allowedVars is the explicit set of variables that may be substituted into .bu files.
var allowedVars = map[string]bool{
	"SSH_PUBKEY":            true,
	"TS_API_CLIENT_ID":      true,
	"TS_API_CLIENT_SECRET":  true,
	"TAILNET_DOMAIN":        true,
	"QUADSYNC_GIT_URL":      true,
	"QUADSYNC_GIT_BRANCH":   true,
	"STORAGE_SMB_HOST":      true,
	"STORAGE_SMB_SHARE":     true,
	"STORAGE_SMB_USER":      true,
	"STORAGE_SMB_PASSWORD":  true,
}

// parseEnv reads a site.env file and returns a map of KEY=VALUE pairs.
// It rejects lines that are not simple KEY=VALUE assignments.
func parseEnv(data string) (map[string]string, error) {
	vars := make(map[string]string)
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("site.env line %d: not a KEY=VALUE assignment: %q", i+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("site.env line %d: empty key", i+1)
		}
		// Reject keys with characters that aren't valid env var names
		for _, c := range key {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				return nil, fmt.Errorf("site.env line %d: invalid character %q in key %q", i+1, string(c), key)
			}
		}
		if !allowedVars[key] {
			return nil, fmt.Errorf("site.env line %d: unknown variable %q (not in allowlist)", i+1, key)
		}
		// Strip optional surrounding quotes from value
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		vars[key] = value
	}
	return vars, nil
}

// substitute replaces only ${KEY} patterns for allowlisted keys.
// Bare $VAR, $(...), and unknown ${VAR} are all left untouched.
func substitute(content string, vars map[string]string) string {
	for key, val := range vars {
		content = strings.ReplaceAll(content, "${"+key+"}", val)
	}
	return content
}

// runButane pipes content through `butane --strict --files-dir <dir>` and returns the output.
func runButane(content string, filesDir string) ([]byte, error) {
	cmd := exec.Command("butane", "--strict", "--files-dir", filesDir)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("butane: %w", err)
	}
	return out, nil
}

// mergeIgnition merges a server ignition JSON into a base ignition JSON.
// Arrays under storage (files, directories, links) are concatenated.
// Users, groups, and units are concatenated then grouped by name (later entries win on conflict).
func mergeIgnition(base, server []byte) ([]byte, error) {
	var b, s map[string]any
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("parsing base ignition: %w", err)
	}
	if err := json.Unmarshal(server, &s); err != nil {
		return nil, fmt.Errorf("parsing server ignition: %w", err)
	}

	mergeArrayField(b, s, "storage", "files")
	mergeArrayField(b, s, "storage", "directories")
	mergeArrayField(b, s, "storage", "links")
	mergeGroupByName(b, s, "passwd", "users")
	mergeGroupByName(b, s, "passwd", "groups")
	mergeGroupByName(b, s, "systemd", "units")

	return json.MarshalIndent(b, "", "  ")
}

// mergeArrayField appends items from s[section][field] to b[section][field].
func mergeArrayField(b, s map[string]any, section, field string) {
	sSection, ok := s[section].(map[string]any)
	if !ok {
		return
	}
	sArr, ok := sSection[field].([]any)
	if !ok || len(sArr) == 0 {
		return
	}
	bSection, ok := b[section].(map[string]any)
	if !ok {
		bSection = make(map[string]any)
		b[section] = bSection
	}
	bArr, _ := bSection[field].([]any)
	bSection[field] = append(bArr, sArr...)
}

// mergeGroupByName concatenates arrays from b and s, then groups by "name" field.
// Within each group, maps are merged (later values override earlier ones).
func mergeGroupByName(b, s map[string]any, section, field string) {
	sSection, ok := s[section].(map[string]any)
	if !ok {
		return
	}
	sArr, ok := sSection[field].([]any)
	if !ok || len(sArr) == 0 {
		return
	}
	bSection, ok := b[section].(map[string]any)
	if !ok {
		bSection = make(map[string]any)
		b[section] = bSection
	}
	bArr, _ := bSection[field].([]any)

	combined := append(bArr, sArr...)

	// Group by name
	groups := make(map[string]map[string]any)
	var order []string
	for _, item := range combined {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		if _, exists := groups[name]; !exists {
			order = append(order, name)
			groups[name] = make(map[string]any)
		}
		for k, v := range m {
			groups[name][k] = v
		}
	}

	result := make([]any, 0, len(order))
	for _, name := range order {
		result = append(result, groups[name])
	}
	bSection[field] = result
}

func run() error {
	envData, err := os.ReadFile("site.env")
	if err != nil {
		return fmt.Errorf("site.env: %w\nCopy site.env.example to site.env and fill in your values.", err)
	}

	if _, err := os.Stat("deploy_key"); err != nil {
		return fmt.Errorf("deploy_key: %w\nPlace your SSH deploy key at deploy_key.", err)
	}

	vars, err := parseEnv(string(envData))
	if err != nil {
		return err
	}

	// Check all required variables are present
	for key := range allowedVars {
		if _, ok := vars[key]; !ok {
			return fmt.Errorf("site.env: missing required variable %q", key)
		}
	}

	buData, err := os.ReadFile("tailpod.bu")
	if err != nil {
		return fmt.Errorf("reading tailpod.bu: %w", err)
	}

	substituted := substitute(string(buData), vars)
	baseIgn, err := runButane(substituted, ".")
	if err != nil {
		return fmt.Errorf("processing tailpod.bu: %w", err)
	}

	// Remove existing output so WriteFile creates fresh with 0600 permissions
	os.Remove("tailpod.ign")

	// Check for optional server.bu overlay
	serverBu, err := os.ReadFile("server.bu")
	if err == nil {
		serverSubstituted := substitute(string(serverBu), vars)
		serverIgn, err := runButane(serverSubstituted, ".")
		if err != nil {
			return fmt.Errorf("processing server.bu: %w", err)
		}

		merged, err := mergeIgnition(baseIgn, serverIgn)
		if err != nil {
			return err
		}

		if err := os.WriteFile("tailpod.ign", merged, 0600); err != nil {
			return err
		}
		fmt.Println("Generated tailpod.ign (with server.bu)")
	} else if os.IsNotExist(err) {
		if err := os.WriteFile("tailpod.ign", baseIgn, 0600); err != nil {
			return err
		}
		fmt.Println("Generated tailpod.ign")
	} else {
		return fmt.Errorf("reading server.bu: %w", err)
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
