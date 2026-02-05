package main

import (
	"strings"
	"testing"
)

func TestParseINI(t *testing.T) {
	input := `[Unit]
Description=test unit
After=network-online.target

[Container]
Image=docker.io/library/nginx:latest
ContainerName=nginx-demo

[Service]
# a comment
Restart=on-failure

[Install]
WantedBy=default.target
`
	f, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseINI error: %v", err)
	}

	// Should have 5 sections: preamble (empty) + Unit + Container + Service + Install
	if len(f.Sections) != 5 {
		t.Fatalf("expected 5 sections, got %d", len(f.Sections))
	}

	unit := f.GetSection("Unit")
	if unit == nil {
		t.Fatal("Unit section not found")
	}
	if !unit.HasKey("Description") {
		t.Error("Unit should have Description key")
	}

	container := f.GetSection("Container")
	if container == nil {
		t.Fatal("Container section not found")
	}
	if !container.HasKey("Image") {
		t.Error("Container should have Image key")
	}
	if !container.HasKey("ContainerName") {
		t.Error("Container should have ContainerName key")
	}

	svc := f.GetSection("Service")
	if svc == nil {
		t.Fatal("Service section not found")
	}
	if !svc.HasKey("Restart") {
		t.Error("Service should have Restart key")
	}
}

func TestParseINIEmpty(t *testing.T) {
	f, err := ParseINI(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseINI error: %v", err)
	}
	// Just the empty preamble section
	if len(f.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(f.Sections))
	}
}

func TestINIRoundTrip(t *testing.T) {
	input := `[Unit]
Description=test

[Container]
Image=nginx:latest
ContainerName=test

[Install]
WantedBy=default.target
`
	f, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseINI error: %v", err)
	}
	output := f.String()
	if output != input {
		t.Errorf("round-trip mismatch:\n--- input ---\n%s\n--- output ---\n%s", input, output)
	}
}

func TestINICommentsPreserved(t *testing.T) {
	input := `[Service]
# This is a comment
Restart=on-failure
`
	f, err := ParseINI(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseINI error: %v", err)
	}
	output := f.String()
	if !strings.Contains(output, "# This is a comment") {
		t.Error("comment not preserved in output")
	}
}
