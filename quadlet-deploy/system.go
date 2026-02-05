package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// run executes a command and returns combined output.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running %s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// gitClone clones a repo.
func gitClone(url, dest, branch string) error {
	_, err := run("git", "clone", "--branch", branch, "--single-branch", "--depth=1", url, dest)
	return err
}

// gitFetch fetches and returns whether there are new changes.
func gitFetch(repoDir, branch string) (bool, error) {
	cmd := exec.Command("git", "fetch", "origin", branch)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git fetch: %w\n%s", err, out)
	}

	// Compare HEAD with FETCH_HEAD
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoDir
	headOut, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	cmd = exec.Command("git", "rev-parse", "FETCH_HEAD")
	cmd.Dir = repoDir
	fetchOut, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse FETCH_HEAD: %w", err)
	}

	return strings.TrimSpace(string(headOut)) != strings.TrimSpace(string(fetchOut)), nil
}

// gitResetHard resets repo to origin/branch.
func gitResetHard(repoDir, branch string) error {
	cmd := exec.Command("git", "reset", "--hard", "origin/"+branch)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git reset: %w\n%s", err, out)
	}
	return nil
}

// createUser creates a system user in the given group.
func createUser(name, group string) error {
	_, err := run("useradd", "--system", "--create-home", "-s", "/sbin/nologin", "-G", group, name)
	if err != nil {
		return fmt.Errorf("creating user %s: %w", name, err)
	}
	_, err = run("loginctl", "enable-linger", name)
	if err != nil {
		return fmt.Errorf("enabling linger for %s: %w", name, err)
	}
	return nil
}

// deleteUser stops services and removes a user.
func deleteUser(name string) error {
	// Disable linger
	if _, err := run("loginctl", "disable-linger", name); err != nil {
		log.Printf("warning: disable-linger %s: %v", name, err)
	}
	// Stop user slice
	uidStr, err := run("id", "-u", name)
	if err == nil {
		uid := strings.TrimSpace(uidStr)
		if _, err := run("systemctl", "stop", "user-"+uid+".slice"); err != nil {
			log.Printf("warning: stop user slice %s: %v", name, err)
		}
	}
	// Remove user and home
	_, err = run("userdel", "-r", name)
	return err
}

// writeQuadlet writes a .container file to the user's quadlet directory.
func writeQuadlet(username, containerName, content string) error {
	home, err := userHome(username)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "containers", "systemd")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating quadlet dir: %w", err)
	}
	path := filepath.Join(dir, containerName+".container")
	return os.WriteFile(path, []byte(content), 0644)
}

// removeQuadlet removes a .container file from the user's quadlet directory.
func removeQuadlet(username, containerName string) error {
	home, err := userHome(username)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".config", "containers", "systemd", containerName+".container")
	return os.Remove(path)
}

// daemonReload runs systemctl --user daemon-reload for a user.
func daemonReload(username string) error {
	_, err := run("systemctl", "--user", "-M", username+"@", "daemon-reload")
	return err
}

// restartService restarts a user service.
func restartService(username, serviceName string) error {
	_, err := run("systemctl", "--user", "-M", username+"@", "restart", serviceName+".service")
	return err
}

// stopService stops a user service.
func stopService(username, serviceName string) error {
	_, err := run("systemctl", "--user", "-M", username+"@", "stop", serviceName+".service")
	return err
}

// managedUsers returns the list of users in the given group.
func managedUsers(group string) ([]string, error) {
	out, err := run("getent", "group", group)
	if err != nil {
		// Group might not exist yet or have no members
		if strings.Contains(err.Error(), "exit status 2") {
			return nil, nil
		}
		return nil, err
	}
	parts := strings.Split(strings.TrimSpace(out), ":")
	if len(parts) < 4 || parts[3] == "" {
		return nil, nil
	}
	return strings.Split(parts[3], ","), nil
}

func userHome(username string) (string, error) {
	// On FCOS, system users created with --create-home get /home/<name>
	return "/home/" + username, nil
}
