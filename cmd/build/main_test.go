package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func validEnv() string {
	return `SSH_PUBKEY="ssh-ed25519 AAAA test@example.com"
QUADSYNC_GIT_URL=git@github.com:org/repo.git
QUADSYNC_GIT_BRANCH=main
`
}

func validEnvWithTailscale() string {
	return validEnv() + `TS_API_CLIENT_ID=client-id
TS_API_CLIENT_SECRET=client-secret
TAILNET_DOMAIN=example.ts.net
`
}

func validEnvWithStorage() string {
	return validEnv() + `STORAGE_SMB_HOST=storage.example.com
STORAGE_SMB_SHARE=backup
STORAGE_SMB_USER=user
STORAGE_SMB_PASSWORD=password
`
}

func validEnvFull() string {
	return validEnvWithTailscale() + `STORAGE_SMB_HOST=storage.example.com
STORAGE_SMB_SHARE=backup
STORAGE_SMB_USER=user
STORAGE_SMB_PASSWORD=password
`
}

func TestParseEnvValid(t *testing.T) {
	vars, err := parseEnv(validEnv())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := vars["SSH_PUBKEY"]; got != "ssh-ed25519 AAAA test@example.com" {
		t.Errorf("SSH_PUBKEY = %q, want unquoted value", got)
	}
	if len(vars) != 3 {
		t.Errorf("got %d vars, want 3", len(vars))
	}
}

func TestParseEnvWithTailscaleVars(t *testing.T) {
	vars, err := parseEnv(validEnvWithTailscale())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vars) != 6 {
		t.Errorf("got %d vars, want 6", len(vars))
	}
	if got := vars["TS_API_CLIENT_ID"]; got != "client-id" {
		t.Errorf("TS_API_CLIENT_ID = %q", got)
	}
}

func TestParseEnvWithStorageVars(t *testing.T) {
	vars, err := parseEnv(validEnvWithStorage())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vars) != 7 {
		t.Errorf("got %d vars, want 7", len(vars))
	}
	if got := vars["STORAGE_SMB_HOST"]; got != "storage.example.com" {
		t.Errorf("STORAGE_SMB_HOST = %q", got)
	}
}

func TestParseEnvWithAllVars(t *testing.T) {
	vars, err := parseEnv(validEnvFull())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vars) != 10 {
		t.Errorf("got %d vars, want 10", len(vars))
	}
}

func TestParseEnvSkipsBlanksAndComments(t *testing.T) {
	input := `# This is a comment

SSH_PUBKEY=key
# Another comment
QUADSYNC_GIT_URL=url
QUADSYNC_GIT_BRANCH=main
`
	vars, err := parseEnv(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vars) != 3 {
		t.Errorf("got %d vars, want 3", len(vars))
	}
}

func TestParseEnvRejectsMalformedLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // substring of error
	}{
		{
			name:  "no equals sign",
			input: "SSH_PUBKEY",
			want:  "not a KEY=VALUE",
		},
		{
			name:  "shell command",
			input: "export FOO=bar",
			want:  "invalid character",
		},
		{
			name:  "function definition",
			input: "my_func() { echo hi; }",
			want:  "not a KEY=VALUE",
		},
		{
			name:  "unknown variable",
			input: "UNKNOWN_VAR=value",
			want:  "not in allowlist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseEnv(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestSubstituteReplacesAllowlistedVars(t *testing.T) {
	vars := map[string]string{
		"TAILNET_DOMAIN": "example.ts.net",
		"SSH_PUBKEY":     "ssh-ed25519 AAAA",
	}
	input := "dns-search=${TAILNET_DOMAIN}\nkey: ${SSH_PUBKEY}"
	got := substitute(input, vars)
	want := "dns-search=example.ts.net\nkey: ssh-ed25519 AAAA"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstituteLeavesUnknownVarsIntact(t *testing.T) {
	vars := map[string]string{
		"TAILNET_DOMAIN": "example.ts.net",
	}
	// These patterns must survive substitution unchanged
	input := `NAME="$(basename "$1")"
echo $NAME
echo ${NAME}
mkdir -p /var/mnt/$NAME
${TAILNET_DOMAIN}`

	got := substitute(input, vars)

	// Unknown vars left as-is
	if !strings.Contains(got, `$NAME`) {
		t.Error("bare $NAME was modified")
	}
	if !strings.Contains(got, `${NAME}`) {
		t.Error("${NAME} was modified")
	}
	if !strings.Contains(got, `$(basename`) {
		t.Error("$(...) was modified")
	}
	// Allowlisted var was replaced
	if strings.Contains(got, "${TAILNET_DOMAIN}") {
		t.Error("${TAILNET_DOMAIN} was not substituted")
	}
	if !strings.Contains(got, "example.ts.net") {
		t.Error("TAILNET_DOMAIN value not found in output")
	}
}

func TestSubstituteMultipleOccurrences(t *testing.T) {
	vars := map[string]string{"TAILNET_DOMAIN": "example.ts.net"}
	input := "${TAILNET_DOMAIN} and ${TAILNET_DOMAIN}"
	got := substitute(input, vars)
	want := "example.ts.net and example.ts.net"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMergeIgnitionFiles(t *testing.T) {
	base := `{
  "ignition": {"version": "3.4.0"},
  "storage": {
    "files": [{"path": "/etc/base"}],
    "directories": [{"path": "/var/base"}]
  },
  "passwd": {
    "users": [{"name": "core", "sshAuthorizedKeys": ["key1"]}]
  },
  "systemd": {
    "units": [{"name": "base.service", "enabled": true}]
  }
}`

	server := `{
  "ignition": {"version": "3.4.0"},
  "storage": {
    "files": [{"path": "/etc/server"}]
  },
  "passwd": {
    "users": [{"name": "core", "groups": ["docker"]}]
  },
  "systemd": {
    "units": [{"name": "server.service", "enabled": true}]
  }
}`

	merged, err := mergeIgnition([]byte(base), []byte(server))
	if err != nil {
		t.Fatalf("merge error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Check files were concatenated
	storage := result["storage"].(map[string]any)
	files := storage["files"].([]any)
	if len(files) != 2 {
		t.Errorf("got %d files, want 2", len(files))
	}

	// Check users were merged by name
	passwd := result["passwd"].(map[string]any)
	users := passwd["users"].([]any)
	if len(users) != 1 {
		t.Errorf("got %d users, want 1 (merged by name)", len(users))
	}
	coreUser := users[0].(map[string]any)
	if _, ok := coreUser["sshAuthorizedKeys"]; !ok {
		t.Error("merged user missing sshAuthorizedKeys from base")
	}
	if _, ok := coreUser["groups"]; !ok {
		t.Error("merged user missing groups from server")
	}

	// Check units were merged
	systemd := result["systemd"].(map[string]any)
	units := systemd["units"].([]any)
	if len(units) != 2 {
		t.Errorf("got %d units, want 2", len(units))
	}
}

func TestMergeIgnitionEmptyServer(t *testing.T) {
	base := `{"ignition": {"version": "3.4.0"}, "storage": {"files": [{"path": "/etc/base"}]}}`
	server := `{"ignition": {"version": "3.4.0"}}`

	merged, err := mergeIgnition([]byte(base), []byte(server))
	if err != nil {
		t.Fatalf("merge error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(merged, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	storage := result["storage"].(map[string]any)
	files := storage["files"].([]any)
	if len(files) != 1 {
		t.Errorf("got %d files, want 1", len(files))
	}
}

func TestParseEnvStripsSingleQuotes(t *testing.T) {
	input := `SSH_PUBKEY='ssh-ed25519 AAAA'
QUADSYNC_GIT_URL=url
QUADSYNC_GIT_BRANCH=main
`
	vars, err := parseEnv(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := vars["SSH_PUBKEY"]; got != "ssh-ed25519 AAAA" {
		t.Errorf("SSH_PUBKEY = %q, want unquoted", got)
	}
}

func TestParseEnvMissingRequired(t *testing.T) {
	// Only one variable — should fail validation in run(), but parseEnv itself just parses what's given.
	// This test verifies parseEnv accepts a subset (validation is in run()).
	input := "SSH_PUBKEY=key\n"
	vars, err := parseEnv(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vars) != 1 {
		t.Errorf("got %d vars, want 1", len(vars))
	}
}
