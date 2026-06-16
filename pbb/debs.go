package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckAndInstallDependencies verifies dependencies on the host and installs missing ones into the isolated root
func CheckAndInstallDependencies(deps []string, currentMirror, branchName string) error {
	if len(deps) == 0 {
		fmt.Println("[pbb] Package has no external dependencies.")
		return nil
	}

	fmt.Printf("[pbb] Found build dependencies: %v\n", deps)
	fmt.Println("[pbb] Verifying and installing missing components...")

	// Base system packages (Arch meta-packages) to exclude from isolation
	systemMetaPackages := map[string]bool{
		"python":    true,
		"python3":   true,
		"coreutils": true,
		"git":       true,
		"texinfo":   true,
		"glibc":     true,
		"gcc":       true,
		"make":      true,
		"bash":      true,
		"sh":        true,
		"sed":       true,
		"awk":       true,
		"grep":      true,
		"tar":       true,
		"curl":      true,
	}

	// Mapping specific Arch package names to binary names for LookPath verification
	packageMap := map[string]string{
		"python3":         "python3",
		"python-pip":      "pip",
		"git-core":        "git",
		"glibc-devel":     "ldconfig", // Used to check runtime/compiler presence
		"xorg-server-dev": "Xorg",
	}

	for _, dep := range deps {
		// Strip version constraints (e.g., "bash>=5.0" -> "bash")
		cleanDep := strings.FieldsFunc(dep, func(r rune) bool {
			return r == '>' || r == '=' || r == '<'
		})[0]

		lookupName := cleanDep
		if mappedName, exists := packageMap[cleanDep]; exists {
			lookupName = mappedName
		}

		// Skip if the package belongs to the base system blocklist
		if systemMetaPackages[cleanDep] {
			if verbose {
				fmt.Printf("[v] Host component '%s' is blocklisted. Skipping.\n", cleanDep)
			}
			continue
		}

		// Skip if the binary is already available in the host system's PATH
		if _, err := exec.LookPath(lookupName); err == nil {
			if verbose {
				fmt.Printf("[v] Dependency '%s' (binary: %s) found on host. Skipping.\n", cleanDep, lookupName)
			}
			continue
		}

		// Skip if the dependency is already installed locally in pbb environment
		localDb, err := readLocalDb()
		if err == nil {
			if _, alreadyInstalled := localDb[cleanDep]; alreadyInstalled {
				fmt.Printf("[pbb] Dependency '%s' is already integrated. Skipping.\n", cleanDep)
				continue
			}
		}

		// Download package from Arch repositories if missing from both host and local env
		fmt.Printf("[pbb] Installing missing dependency: %s\n", cleanDep)

		repoType, pkgFilename, pkgVersion, err := searchPackageInRepositories(cleanDep)
		if err != nil {
			fmt.Printf("[!] Warning: Dependency '%s' not found in repositories. It might be an AUR-only package.\n", cleanDep)
			continue
		}

		url := fmt.Sprintf("%s/%s/os/x86_64/%s", currentMirror, repoType, pkgFilename)
		tmpFile := filepath.Join("/tmp", pkgFilename)

		if verbose {
			fmt.Printf("[v] Downloading dependency %s from URL: %s\n", cleanDep, url)
		}

		if err := downloadFile(url, tmpFile); err != nil {
			return fmt.Errorf("failed to download dependency %s: %v", cleanDep, err)
		}

		// Extract files into TargetPrefix (~/.local/share/pbb/root)
		manifest, err := extractZstTar(tmpFile, TargetPrefix)
		if err != nil {
			os.Remove(tmpFile)
			return fmt.Errorf("failed to extract dependency files for %s: %v", cleanDep, err)
		}

		if err := saveManifest(cleanDep, manifest); err != nil {
			fmt.Printf("[!] Failed to save manifest for %s: %v\n", cleanDep, err)
		}

		registerPackage(cleanDep, pkgVersion, branchName)
		os.Remove(tmpFile)
		fmt.Printf("[+] Dependency '%s' successfully installed locally.\n", cleanDep)
	}

	return nil
}
