package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the deployer configuration.
type Config struct {
	GitURL       string
	GitBranch    string
	TransformDir string
	StateDir     string
	UserGroup    string
	RepoPath     string // derived: StateDir + "/repo"
}

// LoadConfig reads config from an env file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}

	env := parseEnvFile(string(data))
	c := Config{
		GitURL:       env["QDEPLOY_GIT_URL"],
		GitBranch:    env["QDEPLOY_GIT_BRANCH"],
		TransformDir: env["QDEPLOY_TRANSFORM_DIR"],
		StateDir:     env["QDEPLOY_STATE_DIR"],
		UserGroup:    env["QDEPLOY_USER_GROUP"],
	}

	if c.GitURL == "" {
		return Config{}, fmt.Errorf("QDEPLOY_GIT_URL not set in config")
	}
	if c.GitBranch == "" {
		c.GitBranch = "main"
	}
	if c.TransformDir == "" {
		c.TransformDir = "/etc/quadlet-deploy/transforms"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/quadlet-deploy"
	}
	if c.UserGroup == "" {
		c.UserGroup = "cusers"
	}
	c.RepoPath = filepath.Join(c.StateDir, "repo")
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
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			env[key] = value
		}
	}
	return env
}

// Sync performs the full reconciliation: git sync, transform merge, deploy, cleanup.
func Sync(config Config) error {
	// Ensure state dir exists
	if err := os.MkdirAll(config.StateDir, 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	// 1. Git sync
	if _, err := os.Stat(config.RepoPath); os.IsNotExist(err) {
		log.Printf("cloning %s", config.GitURL)
		if err := gitClone(config.GitURL, config.RepoPath, config.GitBranch); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	} else {
		changed, err := gitFetch(config.RepoPath, config.GitBranch)
		if err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
		if changed {
			log.Printf("changes detected, updating")
			if err := gitResetHard(config.RepoPath, config.GitBranch); err != nil {
				return fmt.Errorf("git reset: %w", err)
			}
		}
	}

	// 2. Load transforms
	transforms, err := loadTransforms(config.TransformDir)
	if err != nil {
		return fmt.Errorf("loading transforms: %w", err)
	}

	// 3. Build desired state
	desired, err := buildDesired(config.RepoPath, transforms)
	if err != nil {
		return fmt.Errorf("building desired state: %w", err)
	}

	// 4. Get current managed users
	current, err := managedUsers(config.UserGroup)
	if err != nil {
		return fmt.Errorf("listing managed users: %w", err)
	}
	currentSet := map[string]bool{}
	for _, u := range current {
		currentSet[u] = true
	}

	// 5. Deploy
	hashDir := filepath.Join(config.StateDir, "hashes")
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		return fmt.Errorf("creating hash dir: %w", err)
	}

	for name, content := range desired {
		if !currentSet[name] {
			log.Printf("creating user %s", name)
			if err := createUser(name, config.UserGroup); err != nil {
				log.Printf("error creating user %s: %v", name, err)
				continue
			}
		}

		if !specChanged(hashDir, name, content) {
			log.Printf("%s: unchanged, skipping", name)
			continue
		}

		log.Printf("%s: deploying", name)
		if err := writeQuadlet(name, name, content); err != nil {
			log.Printf("error writing quadlet for %s: %v", name, err)
			continue
		}
		if err := waitForUserManager(name); err != nil {
			log.Printf("error waiting for user manager %s: %v", name, err)
			continue
		}
		if err := daemonReload(name); err != nil {
			log.Printf("error daemon-reload for %s: %v", name, err)
			continue
		}
		if err := restartService(name, name); err != nil {
			log.Printf("error restarting %s: %v", name, err)
			continue
		}
		saveHash(hashDir, name, content)
	}

	// 6. Cleanup: remove containers not in desired
	for _, name := range current {
		if _, exists := desired[name]; !exists {
			log.Printf("%s: removing", name)
			if err := stopService(name, name); err != nil {
				log.Printf("warning: stopping %s: %v", name, err)
			}
			if err := removeQuadlet(name, name); err != nil {
				log.Printf("warning: removing quadlet for %s: %v", name, err)
			}
			if err := deleteUser(name); err != nil {
				log.Printf("error deleting user %s: %v", name, err)
			}
			os.Remove(filepath.Join(hashDir, name))
		}
	}

	return nil
}

// loadTransforms reads all .container files from the transform directory.
// Returns a map of directory name → parsed INI.
func loadTransforms(dir string) (map[string]*INIFile, error) {
	transforms := map[string]*INIFile{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return transforms, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".container") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".container")
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading transform %s: %w", entry.Name(), err)
		}
		f, err := ParseINI(strings.NewReader(string(data)))
		if err != nil {
			return nil, fmt.Errorf("parsing transform %s: %w", entry.Name(), err)
		}
		transforms[name] = f
	}

	return transforms, nil
}

// buildDesired scans the repo and builds the desired state map.
func buildDesired(repoPath string, transforms map[string]*INIFile) (map[string]string, error) {
	desired := map[string]string{}

	// Root-level .container files — no transform
	rootFiles, err := filepath.Glob(filepath.Join(repoPath, "*.container"))
	if err != nil {
		return nil, err
	}
	for _, f := range rootFiles {
		name := strings.TrimSuffix(filepath.Base(f), ".container")
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		desired[name] = string(data)
	}

	// Subdirectories — apply matching transform
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirName := entry.Name()
		transform, ok := transforms[dirName]
		if !ok {
			log.Printf("warning: no transform for directory %s, skipping", dirName)
			continue
		}

		subFiles, err := filepath.Glob(filepath.Join(repoPath, dirName, "*.container"))
		if err != nil {
			return nil, err
		}
		for _, f := range subFiles {
			name := strings.TrimSuffix(filepath.Base(f), ".container")
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", f, err)
			}
			spec, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return nil, fmt.Errorf("parsing %s: %w", f, err)
			}
			merged := MergeTransform(spec, transform)
			desired[name] = merged.String()
		}
	}

	return desired, nil
}

func specChanged(hashDir, name, content string) bool {
	hashFile := filepath.Join(hashDir, name)
	existing, err := os.ReadFile(hashFile)
	if err != nil {
		return true // file doesn't exist, treat as changed
	}
	newHash := sha256Hex(content)
	return strings.TrimSpace(string(existing)) != newHash
}

func saveHash(hashDir, name, content string) {
	hashFile := filepath.Join(hashDir, name)
	os.WriteFile(hashFile, []byte(sha256Hex(content)), 0644)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}
