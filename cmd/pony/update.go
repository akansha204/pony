package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ponyModule = "github.com/akansha204/pony/cmd/pony"

type moduleVersion struct {
	Version string
}

func latestPonyVersion(ctx context.Context) (string, error) {
	url := "https://proxy.golang.org/github.com/akansha204/pony/@latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "pony-update-check")
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version service returned %s", resp.Status)
	}
	var latest moduleVersion
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&latest); err != nil {
		return "", fmt.Errorf("read latest version: %w", err)
	}
	if _, ok := parsePonyVersion(latest.Version); !ok {
		return "", fmt.Errorf("version service returned invalid version %q", latest.Version)
	}
	return latest.Version, nil
}

func updateAvailable(current, latest string) bool {
	currentVersion, currentOK := parsePonyVersion(current)
	latestVersion, latestOK := parsePonyVersion(latest)
	if !latestOK {
		return false
	}
	if !currentOK {
		return true
	}
	return comparePonyVersion(latestVersion, currentVersion) > 0
}

type ponyVersion struct {
	major, minor, patch int
	pre                 string
}

var gitDescribeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-fA-F]+(?:-dirty)?$`)

func parsePonyVersion(value string) (ponyVersion, bool) {
	value = strings.TrimPrefix(value, "v")
	value = gitDescribeSuffix.ReplaceAllString(value, "")
	value = strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(value, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return ponyVersion{}, false
	}
	major, err1 := strconv.Atoi(core[0])
	minor, err2 := strconv.Atoi(core[1])
	patch, err3 := strconv.Atoi(core[2])
	if err1 != nil || err2 != nil || err3 != nil || major < 0 || minor < 0 || patch < 0 {
		return ponyVersion{}, false
	}
	version := ponyVersion{major: major, minor: minor, patch: patch}
	if len(parts) == 2 {
		version.pre = parts[1]
	}
	return version, true
}

func comparePonyVersion(left, right ponyVersion) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.pre == right.pre {
		return 0
	}
	if left.pre == "" {
		return 1
	}
	if right.pre == "" {
		return -1
	}
	if left.pre < right.pre {
		return -1
	}
	return 1
}

func installPonyVersion(version string) error {
	cmd := exec.Command("go", "install", ponyModule+"@"+version)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("go install failed: %s", message)
		}
		return fmt.Errorf("go install failed: %w", err)
	}
	return nil
}

func updatePonyCommand() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	latest, err := latestPonyVersion(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("could not check the latest release: %w", err)
	}
	current := currentVersion()
	if !updateAvailable(current, latest) {
		fmt.Printf("Pony %s is already up to date (latest release: %s).\n", current, latest)
		return nil
	}
	fmt.Printf("Installing Pony %s (currently %s)...\n", latest, current)
	if err := installPonyVersion(latest); err != nil {
		return err
	}
	fmt.Printf("Pony %s installed. Restart Pony to use the updated version.\n", latest)
	return nil
}

func updatePonyVersion(version string) error {
	return installPonyVersion(version)
}

func updateCheckMessage() (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	latest, err := latestPonyVersion(ctx)
	if err != nil {
		return "", false, err
	}
	return latest, updateAvailable(currentVersion(), latest), nil
}
