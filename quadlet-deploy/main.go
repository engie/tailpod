package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const configPath = "/etc/quadlet-deploy/config.env"

func main() {
	log.SetFlags(0)
	log.SetPrefix("quadlet-deploy: ")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "sync":
		cmdSync()
	case "check":
		cmdCheck()
	case "augment":
		cmdAugment()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  quadlet-deploy sync              Full reconcile (git-sync, merge, deploy)")
	fmt.Fprintln(os.Stderr, "  quadlet-deploy check <dir>       Validate .container files")
	fmt.Fprintln(os.Stderr, "  quadlet-deploy augment <file>    Print merged result to stdout")
}

func cmdSync() {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := Sync(cfg); err != nil {
		log.Fatalf("sync failed: %v", err)
	}
}

func cmdCheck() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: quadlet-deploy check <dir>")
		os.Exit(2)
	}
	dir := os.Args[2]
	errs := CheckDir(dir)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}
	fmt.Println("All checks passed.")
}

func cmdAugment() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: quadlet-deploy augment <file>")
		os.Exit(2)
	}

	filePath := os.Args[2]
	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("reading %s: %v", filePath, err)
	}

	spec, err := ParseINI(strings.NewReader(string(data)))
	if err != nil {
		log.Fatalf("parsing %s: %v", filePath, err)
	}

	// Load config to find transform dir
	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	transforms, err := loadTransforms(cfg.TransformDir)
	if err != nil {
		log.Fatalf("loading transforms: %v", err)
	}

	// Determine which transform to apply based on the file's parent directory
	// If it's in a subdirectory that has a matching transform, apply it
	dir := parentDirName(filePath)
	transform, ok := transforms[dir]
	if !ok {
		// No transform, print as-is
		fmt.Print(spec.String())
		return
	}

	merged := MergeTransform(spec, transform)
	fmt.Print(merged.String())
}

func parentDirName(path string) string {
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return ""
}
