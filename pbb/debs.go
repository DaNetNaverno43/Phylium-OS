package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckAndInstallDependencies verifies dependencies on the host and installs
// missing ones into targetPrefix. For AUR packages this should be AurTargetPrefix
// so their dependencies stay isolated from official repo packages.
func CheckAndInstallDependencies(deps []string, currentMirror, branchName, targetPrefix string) error {
	if len(deps) == 0 {
		fmt.Println("[pbb] Package has no external dependencies.")
		return nil
	}

	fmt.Printf("[pbb] Found build dependencies: %v\n", deps)
	fmt.Println("[pbb] Verifying and installing missing components...")

	// Base system packages that are expected to exist on the host — skip isolation for these.
	// These are tools the build process itself needs (bash, make, gcc, etc.) and that
	// pbb cannot reasonably provide in a userspace prefix.
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

	// Maps Arch package names to the binary name we can probe with LookPath.
	packageBinaryMap := map[string]string{
		"python3":         "python3",
		"python-pip":      "pip",
		"git-core":        "git",
		"glibc-devel":     "ldconfig",
		"xorg-server-dev": "Xorg",
	}

	for _, dep := range deps {
		// Strip version constraints: "bash>=5.0" -> "bash", "curl<=8.0" -> "curl"
		cleanDep := strings.FieldsFunc(dep, func(r rune) bool {
			return r == '>' || r == '=' || r == '<'
		})[0]

		// Skip meta packages that must come from the host system
		if systemMetaPackages[cleanDep] {
			if verbose {
				fmt.Printf("[v] Skipping host system package '%s'.\n", cleanDep)
			}
			continue
		}

		// Resolve the binary name to probe on the host PATH
		lookupName := cleanDep
		if mappedName, exists := packageBinaryMap[cleanDep]; exists {
			lookupName = mappedName
		}

		// If the binary is already reachable on the host, no need to install
		if _, err := exec.LookPath(lookupName); err == nil {
			if verbose {
				fmt.Printf("[v] Dependency '%s' (binary: %s) already on host PATH. Skipping.\n", cleanDep, lookupName)
			}
			continue
		}

		// If already installed in the pbb local database, skip
		localDb, err := readLocalDb()
		if err == nil {
			if _, alreadyInstalled := localDb[cleanDep]; alreadyInstalled {
				fmt.Printf("[pbb] Dependency '%s' already installed in pbb. Skipping.\n", cleanDep)
				continue
			}
		}

		fmt.Printf("[pbb] Installing missing dependency: %s\n", cleanDep)

		repoType, pkgFilename, pkgVersion, err := searchPackageInRepositories(cleanDep)
		if err != nil {
			fmt.Printf("[!] Warning: dependency '%s' not found in repositories (may be AUR-only or host-provided).\n", cleanDep)
			continue
		}

		url := fmt.Sprintf("%s/%s/os/x86_64/%s", currentMirror, repoType, pkgFilename)
		tmpFile := filepath.Join("/tmp", pkgFilename)

		if verbose {
			fmt.Printf("[v] Downloading %s from: %s\n", cleanDep, url)
		}

		if err := downloadFile(url, tmpFile); err != nil {
			return fmt.Errorf("failed to download dependency '%s': %v", cleanDep, err)
		}

		// Determine the correct symlink dir based on which prefix we're installing into
		symlinkDir := BinSymlinkDir
		if targetPrefix == AurTargetPrefix {
			symlinkDir = AurBinSymlinkDir
		}

		manifest, err := extractZstTar(tmpFile, targetPrefix, symlinkDir)
		if err != nil {
			os.Remove(tmpFile)
			return fmt.Errorf("failed to extract dependency '%s': %v", cleanDep, err)
		}

		if err := saveManifest(cleanDep, manifest); err != nil {
			fmt.Printf("[!] Failed to save manifest for '%s': %v\n", cleanDep, err)
		}

		source := "repo"
		if targetPrefix == AurTargetPrefix {
			source = "aur"
		}
		registerPackage(cleanDep, pkgVersion, branchName, source)
		os.Remove(tmpFile)
		fmt.Printf("[+] Dependency '%s' installed successfully.\n", cleanDep)
	}

	return nil
}
