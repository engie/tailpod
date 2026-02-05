package main

import (
	"strings"
	"testing"
)

func parseINI(t *testing.T, s string) *INIFile {
	t.Helper()
	f, err := ParseINI(strings.NewReader(s))
	if err != nil {
		t.Fatalf("ParseINI error: %v", err)
	}
	return f
}

func TestMergeBasicDefaults(t *testing.T) {
	spec := parseINI(t, `[Container]
Image=docker.io/library/nginx:latest
ContainerName=nginx-demo

[Install]
WantedBy=default.target
`)

	transform := parseINI(t, `[Unit]
After=network-online.target

[Container]
Network=slirp4netns
PodmanArgs=--network-cmd-path=/usr/local/bin/ts4nsnet

[Service]
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=default.target
`)

	result := MergeTransform(spec, transform)
	output := result.String()

	// Spec values preserved
	if !strings.Contains(output, "Image=docker.io/library/nginx:latest") {
		t.Error("spec Image should be preserved")
	}
	if !strings.Contains(output, "ContainerName=nginx-demo") {
		t.Error("spec ContainerName should be preserved")
	}

	// Transform defaults added
	if !strings.Contains(output, "Network=slirp4netns") {
		t.Error("transform Network should be added")
	}
	if !strings.Contains(output, "PodmanArgs=--network-cmd-path=/usr/local/bin/ts4nsnet") {
		t.Error("transform PodmanArgs should be added")
	}

	// Transform-only section added
	if !strings.Contains(output, "[Unit]") {
		t.Error("transform Unit section should be added")
	}
	if !strings.Contains(output, "After=network-online.target") {
		t.Error("transform After should be added")
	}

	// Service defaults added
	if !strings.Contains(output, "Restart=on-failure") {
		t.Error("transform Restart should be added")
	}
}

func TestMergeSpecTakesPrecedence(t *testing.T) {
	spec := parseINI(t, `[Container]
Image=nginx:latest
Network=host
`)

	transform := parseINI(t, `[Container]
Network=slirp4netns
`)

	result := MergeTransform(spec, transform)
	output := result.String()

	// Spec's Network=host should win over transform's Network=slirp4netns
	if !strings.Contains(output, "Network=host") {
		t.Error("spec Network=host should take precedence")
	}
	if strings.Contains(output, "Network=slirp4netns") {
		t.Error("transform Network=slirp4netns should NOT be present")
	}
}

func TestMergePrependEntries(t *testing.T) {
	spec := parseINI(t, `[Service]
Environment=FOO=bar
`)

	transform := parseINI(t, `[Service]
+ExecStartPre=mkdir -p %t/ts-authkeys
+ExecStartPre=sudo /usr/local/bin/tailpod-mint-key -tag tag:tailpod
Restart=on-failure
`)

	result := MergeTransform(spec, transform)
	output := result.String()

	// Prepended entries should come before spec entries
	prependIdx := strings.Index(output, "ExecStartPre=mkdir")
	envIdx := strings.Index(output, "Environment=FOO=bar")
	if prependIdx < 0 {
		t.Fatal("prepend ExecStartPre=mkdir not found")
	}
	if envIdx < 0 {
		t.Fatal("spec Environment not found")
	}
	if prependIdx > envIdx {
		t.Error("prepend entries should come before spec entries")
	}

	// Both prepend entries present (without + prefix)
	if strings.Contains(output, "+ExecStartPre") {
		t.Error("+ prefix should be stripped from output")
	}
	if !strings.Contains(output, "ExecStartPre=mkdir -p %t/ts-authkeys") {
		t.Error("first prepend entry missing")
	}
	if !strings.Contains(output, "ExecStartPre=sudo /usr/local/bin/tailpod-mint-key -tag tag:tailpod") {
		t.Error("second prepend entry missing")
	}

	// Default also added
	if !strings.Contains(output, "Restart=on-failure") {
		t.Error("transform default Restart should be added")
	}
}

func TestMergeFullExample(t *testing.T) {
	// This mirrors the exact example from the plan
	spec := parseINI(t, `[Container]
Image=docker.io/library/nginx:latest
ContainerName=nginx-demo

[Service]
Environment=FOO=bar

[Install]
WantedBy=default.target
`)

	transform := parseINI(t, `[Unit]
After=network-online.target

[Container]
Network=slirp4netns
PodmanArgs=--network-cmd-path=/usr/local/bin/ts4nsnet --dns=100.100.100.100 --dns-search=tailnet.ts.net

[Service]
+ExecStartPre=mkdir -p %t/ts-authkeys
+ExecStartPre=sudo /usr/local/bin/tailpod-mint-key -config /etc/tailscale/oauth.env -tag tag:tailpod -output %t/ts-authkeys/%N.env -hostname %N
EnvironmentFile=-%t/ts-authkeys/%N.env
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=default.target
`)

	result := MergeTransform(spec, transform)
	output := result.String()

	// Verify section order: Container (from spec, first), Service (from spec), Install (from spec), Unit (from transform, not in spec)
	containerIdx := strings.Index(output, "[Container]")
	serviceIdx := strings.Index(output, "[Service]")
	installIdx := strings.Index(output, "[Install]")
	unitIdx := strings.Index(output, "[Unit]")

	if containerIdx < 0 || serviceIdx < 0 || installIdx < 0 || unitIdx < 0 {
		t.Fatalf("missing sections in output:\n%s", output)
	}

	// Spec sections come first (in spec order), then transform-only sections
	if containerIdx > serviceIdx {
		t.Error("Container should come before Service (spec order)")
	}
	if serviceIdx > installIdx {
		t.Error("Service should come before Install (spec order)")
	}
	// Unit is transform-only, comes after spec sections
	if unitIdx < installIdx {
		t.Error("Unit (transform-only) should come after spec sections")
	}

	// Verify prepend ordering in Service
	mkdirIdx := strings.Index(output, "ExecStartPre=mkdir")
	mintIdx := strings.Index(output, "ExecStartPre=sudo")
	envIdx := strings.Index(output, "Environment=FOO=bar")
	if mkdirIdx > mintIdx {
		t.Error("mkdir should come before mint-key (prepend order)")
	}
	if mintIdx > envIdx {
		t.Error("prepended entries should come before spec entries")
	}

	// Install: spec has WantedBy, so transform's WantedBy is skipped
	wantedCount := strings.Count(output, "WantedBy=default.target")
	if wantedCount != 1 {
		t.Errorf("expected 1 WantedBy in Install, got %d (should deduplicate)", wantedCount)
	}
}

func TestMergeEmptySpec(t *testing.T) {
	spec := parseINI(t, `[Container]
Image=nginx:latest
`)

	transform := parseINI(t, `[Service]
Restart=on-failure
`)

	result := MergeTransform(spec, transform)
	output := result.String()

	if !strings.Contains(output, "[Container]") {
		t.Error("spec Container section should be present")
	}
	if !strings.Contains(output, "[Service]") {
		t.Error("transform Service section should be added")
	}
}

func TestMergeNoTransform(t *testing.T) {
	spec := parseINI(t, `[Container]
Image=nginx:latest

[Install]
WantedBy=default.target
`)

	transform := parseINI(t, "")

	result := MergeTransform(spec, transform)
	output := result.String()

	expected := `[Container]
Image=nginx:latest

[Install]
WantedBy=default.target
`
	if output != expected {
		t.Errorf("no-transform merge mismatch:\n--- expected ---\n%s\n--- got ---\n%s", expected, output)
	}
}
