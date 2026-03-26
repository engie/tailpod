package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// requiredVars must be present in site.env for every build.
var requiredVars = map[string]bool{
	"SSH_PUBKEY":       true,
	"QUADSYNC_GIT_URL": true,
	"QUADSYNC_GIT_BRANCH": true,
}

// overlayVars maps each optional .bu file to the variables it needs.
var overlayVars = map[string][]string{
	"tailscale.bu": {"TS_API_CLIENT_ID", "TS_API_CLIENT_SECRET", "TAILNET_DOMAIN"},
	"server.bu":    {"STORAGE_SMB_HOST", "STORAGE_SMB_SHARE", "STORAGE_SMB_USER", "STORAGE_SMB_PASSWORD"},
}

// overlayOrder controls the order overlays are merged into the base ignition.
// When multiple overlays write to the same file path, their inline contents
// are concatenated (see mergeFileContents). This allows each overlay to
// contribute sections to shared files like _base.container.
var overlayOrder = []string{"tailscale.bu", "server.bu"}

// allowedVars is the union of required and overlay variables (used by parseEnv).
var allowedVars = func() map[string]bool {
	m := make(map[string]bool)
	for k := range requiredVars {
		m[k] = true
	}
	for _, vars := range overlayVars {
		for _, v := range vars {
			m[v] = true
		}
	}
	return m
}()

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

// mergeFileContents concatenates inline file contents for storage.files entries
// that share the same path. When two overlays both write to the same path, their
// contents are appended (with a newline separator) rather than last-writer-wins.
// This allows tailpod.bu and server.bu to each contribute sections to a shared
// transform file like _base.container.
func mergeFileContents(b map[string]any) {
	storage, ok := b["storage"].(map[string]any)
	if !ok {
		return
	}
	files, ok := storage["files"].([]any)
	if !ok {
		return
	}

	seen := make(map[string]int) // path -> index in deduped
	var deduped []any
	for _, item := range files {
		m, ok := item.(map[string]any)
		if !ok {
			deduped = append(deduped, item)
			continue
		}
		path, _ := m["path"].(string)
		if path == "" {
			deduped = append(deduped, item)
			continue
		}
		idx, exists := seen[path]
		if !exists {
			seen[path] = len(deduped)
			deduped = append(deduped, item)
			continue
		}
		// Same path — concatenate contents
		existing := deduped[idx].(map[string]any)
		merged, err := concatDataURI(existing, m)
		if err != nil {
			// Can't merge (e.g. remote URL source) — last writer wins
			deduped[idx] = item
			continue
		}
		deduped[idx] = merged
	}
	storage["files"] = deduped
}

// concatDataURI concatenates the inline contents of two ignition file entries.
// Returns the first entry with the combined content.
func concatDataURI(a, b map[string]any) (map[string]any, error) {
	aText, err := decodeDataURI(a)
	if err != nil {
		return nil, err
	}
	bText, err := decodeDataURI(b)
	if err != nil {
		return nil, err
	}
	// Ensure newline between concatenated sections
	if !strings.HasSuffix(aText, "\n") {
		aText += "\n"
	}
	combined := aText + bText
	// Re-encode as plain data: URI (drop any compression from the originals)
	result := make(map[string]any)
	for k, v := range a {
		result[k] = v
	}
	result["contents"] = map[string]any{
		"source": "data:," + url.PathEscape(combined),
	}
	return result, nil
}

// decodeDataURI extracts the text content from an ignition file entry.
// Handles both plain data: URIs and gzip+base64 compressed ones.
func decodeDataURI(entry map[string]any) (string, error) {
	contents, ok := entry["contents"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("no contents")
	}
	source, ok := contents["source"].(string)
	if !ok {
		return "", fmt.Errorf("no source")
	}
	compression, _ := contents["compression"].(string)

	if !strings.HasPrefix(source, "data:") {
		return "", fmt.Errorf("not a data: URI")
	}

	switch {
	case compression == "" && strings.HasPrefix(source, "data:,"):
		// Plain URL-encoded: data:,content
		decoded, err := url.PathUnescape(strings.TrimPrefix(source, "data:,"))
		if err != nil {
			return "", err
		}
		return decoded, nil

	case compression == "gzip" && strings.HasPrefix(source, "data:;base64,"):
		// Gzip+base64: data:;base64,<base64-gzip>
		b64 := strings.TrimPrefix(source, "data:;base64,")
		compressed, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", err
		}
		gz, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return "", err
		}
		defer gz.Close()
		data, err := io.ReadAll(gz)
		if err != nil {
			return "", err
		}
		return string(data), nil

	default:
		return "", fmt.Errorf("unsupported data URI format")
	}
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
	for key := range requiredVars {
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

	// Merge optional overlays
	var overlayNames []string
	for _, name := range overlayOrder {
		overlayBu, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}

		// Warn about missing vars for this overlay
		for _, key := range overlayVars[name] {
			if _, ok := vars[key]; !ok {
				fmt.Fprintf(os.Stderr, "Warning: %s exists but site.env is missing variable %q\n", name, key)
			}
		}

		overlaySubstituted := substitute(string(overlayBu), vars)
		overlayIgn, err := runButane(overlaySubstituted, ".")
		if err != nil {
			return fmt.Errorf("processing %s: %w", name, err)
		}

		baseIgn, err = mergeIgnition(baseIgn, overlayIgn)
		if err != nil {
			return fmt.Errorf("merging %s: %w", name, err)
		}

		overlayNames = append(overlayNames, name)
	}

	// Merge same-path files by concatenating their inline contents.
	// This allows multiple .bu files to contribute sections to a shared file
	// (e.g. _base.container gets lifecycle from tailpod.bu and storage from server.bu).
	var merged map[string]any
	if err := json.Unmarshal(baseIgn, &merged); err != nil {
		return fmt.Errorf("parsing merged ignition: %w", err)
	}
	mergeFileContents(merged)
	baseIgn, err = json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding merged ignition: %w", err)
	}

	if err := os.WriteFile("tailpod.ign", baseIgn, 0600); err != nil {
		return err
	}

	if len(overlayNames) > 0 {
		fmt.Printf("Generated tailpod.ign (with %s)\n", strings.Join(overlayNames, ", "))
	} else {
		fmt.Println("Generated tailpod.ign")
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
