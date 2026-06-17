package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckAndInstallDependencies verifies build dependencies on the host and installs
// missing ones into targetPrefix. For AUR packages pass AurTargetPrefix so their
// dependencies stay isolated from official repo packages.
func CheckAndInstallDependencies(deps []string, currentMirror, branchName, targetPrefix string) error {
	if len(deps) == 0 {
		fmt.Println("[pbb] Package has no external dependencies.")
		return nil
	}

	fmt.Printf("[pbb] Found build dependencies: %v\n", deps)
	fmt.Println("[pbb] Verifying and installing missing components...")

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

	packageBinaryMap := map[string]string{
		"python3":         "python3",
		"python-pip":      "pip",
		"git-core":        "git",
		"glibc-devel":     "ldconfig",
		"xorg-server-dev": "Xorg",
	}

	symlinkDir := BinSymlinkDir
	if targetPrefix == AurTargetPrefix {
		symlinkDir = AurBinSymlinkDir
	}

	source := "repo"
	if targetPrefix == AurTargetPrefix {
		source = "aur"
	}

	for _, dep := range deps {
		cleanDep := strings.FieldsFunc(dep, func(r rune) bool {
			return r == '>' || r == '=' || r == '<'
		})[0]

		if systemMetaPackages[cleanDep] {
			if verbose {
				fmt.Printf("[v] Skipping host system package '%s'.\n", cleanDep)
			}
			continue
		}

		lookupName := cleanDep
		if mappedName, exists := packageBinaryMap[cleanDep]; exists {
			lookupName = mappedName
		}

		if _, err := exec.LookPath(lookupName); err == nil {
			if verbose {
				fmt.Printf("[v] Dependency '%s' (binary: %s) already on host PATH. Skipping.\n", cleanDep, lookupName)
			}
			continue
		}

		localDb, err := readLocalDb()
		if err == nil {
			if _, alreadyInstalled := localDb[cleanDep]; alreadyInstalled {
				fmt.Printf("[pbb] Dependency '%s' already installed in pbb. Skipping.\n", cleanDep)
				continue
			}
		}

		fmt.Printf("[pbb] Installing missing dependency: %s\n", cleanDep)

		repoType, pkgFilename, pkgVersion, sha256sum, err := searchPackageInRepositories(cleanDep)
		if err != nil {
			fmt.Printf("[!] Warning: dependency '%s' not found in repositories: %v\n", cleanDep, err)
			continue
		}

		url := fmt.Sprintf("%s/%s/os/x86_64/%s", currentMirror, repoType, pkgFilename)

		if verbose {
			fmt.Printf("[v] Downloading %s from: %s\n", cleanDep, url)
		}

		tmpFile, tmpDir, err := downloadToTempDir(url, pkgFilename)
		if err != nil {
			return fmt.Errorf("failed to download dependency '%s': %v", cleanDep, err)
		}

		if err := verifySHA256(tmpFile, sha256sum); err != nil {
			os.RemoveAll(tmpDir)
			return fmt.Errorf("checksum verification failed for dependency '%s': %v", cleanDep, err)
		}

		manifest, err := extractZstTar(tmpFile, targetPrefix, symlinkDir)
		os.RemoveAll(tmpDir)
		if err != nil {
			return fmt.Errorf("failed to extract dependency '%s': %v", cleanDep, err)
		}

		if err := saveManifest(cleanDep, manifest); err != nil {
			fmt.Printf("[!] Failed to save manifest for '%s': %v\n", cleanDep, err)
		}

		if err := registerPackage(cleanDep, pkgVersion, branchName, source); err != nil {
			fmt.Printf("[!] Failed to register dependency '%s' in database: %v\n", cleanDep, err)
		}

		fmt.Printf("[+] Dependency '%s' installed successfully.\n", cleanDep)

		lookupPath := filepath.Join(symlinkDir, lookupName)
		if _, err := os.Stat(lookupPath); err != nil {
			if verbose {
				fmt.Printf("[v] Note: binary '%s' not found at %s after install — may be under a different name.\n", lookupName, lookupPath)
			}
		}
	}

	return nil
}
