package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// --- Package Name Utilities ---

// parsePkgNameFromFilename extracts the package name from an Arch filename like:
//   lib32-mesa-23.1.0-1-x86_64.pkg.tar.zst
// Format: <name>-<pkgver>-<pkgrel>-<arch>.pkg.tar.zst
// We strip the last 3 dash-separated segments from the end (arch, pkgrel, pkgver).
// This correctly handles multi-hyphen names like "lib32-mesa" or "python-requests".
func parsePkgNameFromFilename(filename string) string {
	name := strings.TrimSuffix(filename, ".pkg.tar.zst")
	name = strings.TrimSuffix(name, ".pkg.tar.xz")

	parts := strings.Split(name, "-")
	if len(parts) >= 4 {
		return strings.Join(parts[:len(parts)-3], "-")
	}
	return parts[0]
}

// --- Arch Repository Database Parser ---

// dbPackageInfo holds the fields we extract from a repo .db entry.
type dbPackageInfo struct {
	filename string
	version  string
	sha256   string // %SHA256SUM% field — used to verify downloaded packages
}

// parseDescBlock extracts filename, version and sha256 from a desc file content.
func parseDescBlock(content string) dbPackageInfo {
	var info dbPackageInfo
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i+1 >= len(lines) {
			break
		}
		switch line {
		case "%FILENAME%":
			info.filename = strings.TrimSpace(lines[i+1])
		case "%VERSION%":
			info.version = strings.TrimSpace(lines[i+1])
		case "%SHA256SUM%":
			info.sha256 = strings.TrimSpace(lines[i+1])
		}
	}
	return info
}

func searchPackageInRepositories(targetPkg string) (repoName string, filename string, version string, sha256sum string, err error) {
	repositories := []string{"core", "extra", "multilib"}

	for _, repo := range repositories {
		dbPath := filepath.Join(SyncDir, repo+".db")
		file, err := os.Open(dbPath)
		if err != nil {
			continue
		}

		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return "", "", "", "", fmt.Errorf("failed to open db %s: %v", repo, err)
		}

		tarReader := tar.NewReader(gzipReader)

		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				gzipReader.Close()
				file.Close()
				return "", "", "", "", fmt.Errorf("error reading db %s: %v", repo, err)
			}

			parts := strings.Split(header.Name, "/")
			if len(parts) < 2 || parts[1] != "desc" {
				continue
			}

			// .db directory name format: "<pkgname>-<pkgver>-<pkgrel>"
			// Strip the last two segments to get the package name.
			fullDirName := parts[0]
			var dashIdx []int
			for i := 0; i < len(fullDirName); i++ {
				if fullDirName[i] == '-' {
					dashIdx = append(dashIdx, i)
				}
			}

			var currentPkgName string
			if len(dashIdx) >= 2 {
				currentPkgName = fullDirName[:dashIdx[len(dashIdx)-2]]
			} else {
				currentPkgName = strings.Split(fullDirName, "-")[0]
			}

			if currentPkgName != targetPkg {
				continue
			}

			buf := new(strings.Builder)
			if _, err := io.Copy(buf, tarReader); err != nil {
				gzipReader.Close()
				file.Close()
				return "", "", "", "", fmt.Errorf("error reading desc for %s: %v", targetPkg, err)
			}

			info := parseDescBlock(buf.String())
			if info.filename == "" || info.version == "" {
				// Malformed entry — keep searching
				continue
			}

			gzipReader.Close()
			file.Close()
			return repo, info.filename, info.version, info.sha256, nil
		}

		gzipReader.Close()
		file.Close()
	}

	return "", "", "", "", fmt.Errorf("package '%s' not found in any repository", targetPkg)
}

func searchPackagesGlobal(searchTerm string) error {
	repositories := []string{"core", "extra", "multilib"}
	foundCount := 0

	for _, repo := range repositories {
		dbPath := filepath.Join(SyncDir, repo+".db")
		file, err := os.Open(dbPath)
		if err != nil {
			continue
		}

		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return fmt.Errorf("failed to open db %s: %v", repo, err)
		}

		tarReader := tar.NewReader(gzipReader)
		parsedPackages := make(map[string]bool)

		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				gzipReader.Close()
				file.Close()
				return fmt.Errorf("error reading db %s: %v", repo, err)
			}

			parts := strings.Split(header.Name, "/")
			if len(parts) < 2 || parts[1] != "desc" {
				continue
			}

			fullDirName := parts[0]
			if !strings.Contains(strings.ToLower(fullDirName), strings.ToLower(searchTerm)) {
				continue
			}

			if parsedPackages[fullDirName] {
				continue
			}
			parsedPackages[fullDirName] = true

			buf := new(strings.Builder)
			if _, err := io.Copy(buf, tarReader); err != nil {
				continue
			}

			info := parseDescBlock(buf.String())
			// Also extract description for display
			var pkgDesc string
			lines := strings.Split(buf.String(), "\n")
			for i, line := range lines {
				if line == "%DESC%" && i+1 < len(lines) {
					pkgDesc = strings.TrimSpace(lines[i+1])
				}
				if line == "%NAME%" && i+1 < len(lines) && info.filename == "" {
					// fallback name extraction
					_ = strings.TrimSpace(lines[i+1])
				}
			}

			if info.filename != "" {
				displayName := parsePkgNameFromFilename(info.filename)
				fmt.Printf("\033[1;34m%s/\033[1;32m%s \033[1;37m%s\033[0m\n", repo, displayName, info.version)
				if pkgDesc != "" {
					fmt.Printf("    %s\n", pkgDesc)
				}
				foundCount++
			}
		}
		gzipReader.Close()
		file.Close()
	}

	if foundCount == 0 {
		fmt.Printf("[pbb] No matches found for '%s'.\n", searchTerm)
	} else {
		fmt.Printf("\n[+] Packages found: %d\n", foundCount)
	}

	return nil
}

// --- SHA256 Verification ---

// verifySHA256 computes the SHA256 of the file at path and compares it to expected.
// If expected is empty (not provided by the repo db), verification is skipped with a warning.
func verifySHA256(path, expected string) error {
	if expected == "" {
		fmt.Printf("[!] Warning: no SHA256 checksum available for %s — skipping verification.\n", filepath.Base(path))
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open file for checksum: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("failed to hash file: %v", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return fmt.Errorf("SHA256 mismatch for %s:\n  expected: %s\n  got:      %s", filepath.Base(path), expected, got)
	}

	if verbose {
		fmt.Printf("[v] SHA256 OK: %s\n", filepath.Base(path))
	}
	return nil
}

// --- AUR Extension Functions ---

func searchAur(searchTerm string) error {
	url := fmt.Sprintf("https://aur.archlinux.org/rpc/?v=5&type=search&arg=%s", searchTerm)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to build AUR request: %v", err)
	}
	req.Header.Set("User-Agent", "pbb-Package-Manager/1.0 (RootlessOS; AUR Extension)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network error during AUR query: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AUR server returned status: %s", resp.Status)
	}

	var aurData AurResponse
	if err := json.NewDecoder(resp.Body).Decode(&aurData); err != nil {
		return fmt.Errorf("failed to parse AUR JSON response: %v", err)
	}

	if aurData.ResultCount == 0 {
		fmt.Printf("[\033[1;33mAUR\033[0m] No matches found for '%s'.\n", searchTerm)
		return nil
	}

	fmt.Printf("\n[\033[1;33mAUR\033[0m] Matches found in AUR: %d\n", aurData.ResultCount)
	for _, pkg := range aurData.Results {
		fmt.Printf("\033[1;33maur/\033[1;32m%s \033[1;37m%s\033[0m\n", pkg.Name, pkg.Version)
		if pkg.Description != "" {
			fmt.Printf("    %s\n", pkg.Description)
		}
	}
	return nil
}

// --- Core File Utilities ---

// downloadToTempDir downloads url into a freshly created temp directory,
// returning the path to the downloaded file and the temp dir.
// The caller is responsible for os.RemoveAll(tmpDir) when done.
func downloadToTempDir(url, filename string) (filePath string, tmpDir string, err error) {
	tmpDir, err = os.MkdirTemp("", "pbb-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp directory: %v", err)
	}

	filePath = filepath.Join(tmpDir, filename)
	if err := downloadFile(url, filePath); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", err
	}
	return filePath, tmpDir, nil
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to build request for %s: %v", url, err)
	}
	req.Header.Set("User-Agent", "pbb-Package-Manager/1.0 (RootlessOS; SysAdmin Custom Tool)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network error downloading %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s for %s", resp.Status, url)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %v", dest, err)
	}
	defer out.Close()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to write downloaded data to %s: %v", dest, err)
	}
	return nil
}

func patchBinaryElf(targetPath string) {
	// $ORIGIN-relative entries cover the common case where binary and libs
	// live in the same prefix tree (repo packages in TargetPrefix,
	// or AUR packages in AurTargetPrefix resolving their own deps).
	//
	// The absolute entries at the end cover cross-prefix resolution:
	// an AUR binary needs to find official-repo .so files in TargetPrefix,
	// and an official binary may need AUR-installed libs in AurTargetPrefix.
	//
	// Absolute paths are fine here — both prefixes are already anchored to
	// ~/.local/share/pbb/ so the whole installation is user-specific by design.
	rpath := strings.Join([]string{
		"$ORIGIN/../lib",
		"$ORIGIN/../../usr/lib",
		"$ORIGIN/../../lib",
		"$ORIGIN/../usr/lib",
		filepath.Join(TargetPrefix, "usr", "lib"),
		filepath.Join(TargetPrefix, "usr", "lib32"),
		filepath.Join(AurTargetPrefix, "usr", "lib"),
		filepath.Join(AurTargetPrefix, "usr", "lib32"),
	}, ":")

	cmd := exec.Command("patchelf", "--set-rpath", rpath, targetPath)
	out, err := cmd.CombinedOutput()
	if err != nil && verbose {
		fmt.Printf("[v] patchelf on %s: %v — %s\n", filepath.Base(targetPath), err, strings.TrimSpace(string(out)))
	}
}

// createBinSymlink creates a symlink symlinkDir/basename -> targetPath.
// symlinkDir is passed explicitly so repo and AUR packages use separate dirs.
func createBinSymlink(targetPath, symlinkDir string) string {
	fileName := filepath.Base(targetPath)
	if strings.HasPrefix(fileName, ".") || strings.Contains(targetPath, "/usr/lib/") {
		return ""
	}

	if err := os.MkdirAll(symlinkDir, 0755); err != nil {
		if verbose {
			fmt.Printf("[v] Failed to create symlink dir %s: %v\n", symlinkDir, err)
		}
		return ""
	}

	symlinkPath := filepath.Join(symlinkDir, fileName)
	_ = os.Remove(symlinkPath)
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		if verbose {
			fmt.Printf("[v] Failed to create symlink %s -> %s: %v\n", symlinkPath, targetPath, err)
		}
		return ""
	}
	return symlinkPath
}

// extractZstTar extracts a .pkg.tar.zst archive into targetDir.
// Binaries in /bin/ or /sbin/ get patchelf treatment and a symlink in symlinkDir.
func extractZstTar(archivePath, targetDir, symlinkDir string) ([]string, error) {
	var fileList []string

	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open archive %s: %v", archivePath, err)
	}
	defer file.Close()

	zstdReader, err := zstd.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise zstd reader: %v", err)
	}
	defer zstdReader.Close()

	tarReader := tar.NewReader(zstdReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading archive %s: %v", filepath.Base(archivePath), err)
		}

		// Skip package metadata entries (start with ".")
		if strings.HasPrefix(header.Name, ".") {
			continue
		}

		target := filepath.Join(targetDir, header.Name)
		fileList = append(fileList, target)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil && verbose {
				fmt.Printf("[v] Failed to create directory %s: %v\n", target, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, fmt.Errorf("failed to create parent dirs for %s: %v", target, err)
			}

			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return nil, fmt.Errorf("failed to create file %s: %v", target, err)
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return nil, fmt.Errorf("failed to write file %s: %v", target, err)
			}
			outFile.Close()

			isExecutable := (header.FileInfo().Mode() & 0111) != 0
			isSharedLib := strings.Contains(target, ".so")

			if isExecutable || isSharedLib {
				patchBinaryElf(target)
			}
			if isExecutable && (strings.Contains(target, "/bin/") || strings.Contains(target, "/sbin/")) {
				if sym := createBinSymlink(target, symlinkDir); sym != "" {
					fileList = append(fileList, sym)
				}
			}

		case tar.TypeSymlink:
			_ = os.Remove(target)
			linkName := header.Linkname
			// Rewrite absolute symlinks to be relative to the targetDir prefix
			if strings.HasPrefix(linkName, "/") {
				linkName = filepath.Join(targetDir, linkName)
			}
			if err := os.Symlink(linkName, target); err != nil && verbose {
				fmt.Printf("[v] Failed to create symlink %s -> %s: %v\n", target, linkName, err)
			}
		}
	}

	return fileList, nil
}

// removePackageWithManifest removes all files recorded in a package's manifest
// in reverse order (files before directories), cleans up the manifest and db entry.
func removePackageWithManifest(pkgName string) error {
	manifestPath := filepath.Join(ManifestsDir, pkgName+".list")
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("manifest not found for '%s' — is it installed?", pkgName)
	}
	if err != nil {
		return fmt.Errorf("failed to read manifest for '%s': %v", pkgName, err)
	}

	files := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Remove in reverse order: files before their parent directories
	for i := len(files) - 1; i >= 0; i-- {
		f := strings.TrimSpace(files[i])
		if f == "" || f == "/" {
			continue
		}

		fi, err := os.Lstat(f)
		if err != nil {
			// Already gone — fine
			continue
		}

		if fi.IsDir() {
			// Only remove if empty — shared dirs may contain other packages' files
			if err := os.Remove(f); err != nil && verbose {
				fmt.Printf("[v] Skipping non-empty directory: %s\n", f)
			}
		} else {
			if err := os.Remove(f); err != nil && verbose {
				fmt.Printf("[v] Failed to remove %s: %v\n", f, err)
			}
		}
	}

	if err := os.Remove(manifestPath); err != nil && verbose {
		fmt.Printf("[v] Failed to remove manifest %s: %v\n", manifestPath, err)
	}

	if err := unregisterPackage(pkgName); err != nil {
		return fmt.Errorf("removed files but failed to update database: %v", err)
	}

	fmt.Printf("[+] Package '%s' removed.\n", pkgName)
	return nil
}

// --- System Directories & State ---

func initSystemDirs() error {
	paths := []string{
		PbbDir, ManifestsDir, SyncDir,
		TargetPrefix, AurTargetPrefix,
		BinSymlinkDir, AurBinSymlinkDir,
		InitdDir, LogDir,
	}
	for _, p := range paths {
		if err := os.MkdirAll(p, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", p, err)
		}
	}
	if _, err := os.Stat(LocalDbPath); os.IsNotExist(err) {
		if err := os.WriteFile(LocalDbPath, []byte("{}"), 0644); err != nil {
			return fmt.Errorf("failed to initialise local database: %v", err)
		}
	}
	return nil
}

func saveManifest(pkgName string, files []string) error {
	content := strings.Join(files, "\n")
	path := filepath.Join(ManifestsDir, pkgName+".list")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write manifest for '%s': %v", pkgName, err)
	}
	return nil
}

func GetOrUpdateSnapshot() (string, error) {
	now := time.Now()
	file, err := os.ReadFile(StateFilePath)

	if os.IsNotExist(err) {
		initialDate := now.AddDate(0, 0, -14)
		state := PbbState{CurrentSnapshot: initialDate.Format("2006/01/02"), LastUpdated: now}
		return state.CurrentSnapshot, saveState(state)
	}
	if err != nil {
		return "", fmt.Errorf("failed to read state file: %v", err)
	}

	var state PbbState
	if err := json.Unmarshal(file, &state); err != nil {
		return "", fmt.Errorf("failed to parse state file: %v", err)
	}

	if now.Sub(state.LastUpdated) >= SnapshotDuration {
		parsedTime, err := time.Parse("2006/01/02", state.CurrentSnapshot)
		if err != nil {
			return "", fmt.Errorf("failed to parse snapshot date '%s': %v", state.CurrentSnapshot, err)
		}
		state.CurrentSnapshot = parsedTime.AddDate(0, 0, 14).Format("2006/01/02")
		state.LastUpdated = now
		return state.CurrentSnapshot, saveState(state)
	}
	return state.CurrentSnapshot, nil
}

func saveState(state PbbState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialise state: %v", err)
	}
	return os.WriteFile(StateFilePath, data, 0644)
}

// handleRollback re-downloads the package from the stable archive mirror,
// verifies the checksum, then replaces the installed bleeding-edge version.
func handleRollback(pkgName, stableMirror string) {
	db, err := readLocalDb()
	if err != nil {
		fmt.Printf("[pbb] Failed to read local database: %v\n", err)
		return
	}

	pkg, exists := db[pkgName]
	if !exists {
		fmt.Printf("[pbb] Package '%s' is not installed.\n", pkgName)
		return
	}
	if pkg.Branch != "bleeding" {
		fmt.Printf("[pbb] Package '%s' is already on stable. Nothing to do.\n", pkgName)
		return
	}

	fmt.Printf("[pbb] Rolling back '%s' from bleeding to stable...\n", pkgName)

	repoType, pkgFilename, pkgVersion, sha256sum, err := searchPackageInRepositories(pkgName)
	if err != nil {
		fmt.Printf("[pbb] Could not find '%s' in stable repositories: %v\n", pkgName, err)
		return
	}

	url := fmt.Sprintf("%s/%s/os/x86_64/%s", stableMirror, repoType, pkgFilename)

	tmpFile, tmpDir, err := downloadToTempDir(url, pkgFilename)
	if err != nil {
		fmt.Printf("[pbb] Failed to download stable version of '%s': %v\n", pkgName, err)
		return
	}
	defer os.RemoveAll(tmpDir)

	if err := verifySHA256(tmpFile, sha256sum); err != nil {
		fmt.Printf("[pbb] Checksum verification failed for '%s': %v\n", pkgName, err)
		return
	}

	targetPrefix := TargetPrefix
	symlinkDir := BinSymlinkDir
	if pkg.Source == "aur" {
		targetPrefix = AurTargetPrefix
		symlinkDir = AurBinSymlinkDir
	}

	if err := removePackageWithManifest(pkgName); err != nil && verbose {
		fmt.Printf("[v] Pre-rollback cleanup warning: %v\n", err)
	}

	manifest, err := extractZstTar(tmpFile, targetPrefix, symlinkDir)
	if err != nil {
		fmt.Printf("[pbb] Failed to extract stable package '%s': %v\n", pkgName, err)
		return
	}

	if err := saveManifest(pkgName, manifest); err != nil {
		fmt.Printf("[pbb] Failed to save manifest after rollback: %v\n", err)
	}

	if err := registerPackage(pkgName, pkgVersion, "stable", pkg.Source); err != nil {
		fmt.Printf("[pbb] Failed to update database after rollback: %v\n", err)
	}

	fmt.Printf("[+] Package '%s' rolled back to stable (%s).\n", pkgName, pkgVersion)
}

// --- Local Database ---

func readLocalDb() (map[string]PackageInfo, error) {
	file, err := os.ReadFile(LocalDbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read local db: %v", err)
	}
	var db map[string]PackageInfo
	if err := json.Unmarshal(file, &db); err != nil {
		return nil, fmt.Errorf("failed to parse local db: %v", err)
	}
	if db == nil {
		db = make(map[string]PackageInfo)
	}
	return db, nil
}

func writeLocalDb(db map[string]PackageInfo) error {
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialise local db: %v", err)
	}
	return os.WriteFile(LocalDbPath, data, 0644)
}

func registerPackage(name, version, branch, source string) error {
	db, err := readLocalDb()
	if err != nil {
		return err
	}
	db[name] = PackageInfo{Name: name, Version: version, Branch: branch, Source: source}
	return writeLocalDb(db)
}

func unregisterPackage(name string) error {
	db, err := readLocalDb()
	if err != nil {
		return err
	}
	delete(db, name)
	return writeLocalDb(db)
}

// --- AUR Build Pipeline ---

func downloadAndExtractAurSnapshot(pkgName string) error {
	url := fmt.Sprintf("https://aur.archlinux.org/rpc/?v=5&type=info&arg[]=%s", pkgName)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to build AUR RPC request: %v", err)
	}
	req.Header.Set("User-Agent", "pbb-Package-Manager/1.0 (RootlessOS; AUR Fetcher)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AUR RPC network error: %v", err)
	}
	defer resp.Body.Close()

	var aurData AurResponse
	if err := json.NewDecoder(resp.Body).Decode(&aurData); err != nil {
		return fmt.Errorf("failed to parse AUR RPC response: %v", err)
	}

	if aurData.ResultCount == 0 {
		return fmt.Errorf("package '%s' not found in AUR", pkgName)
	}

	targetPkg := aurData.Results[0]
	if targetPkg.URLPath == "" {
		return fmt.Errorf("AUR returned no snapshot URL for '%s'", pkgName)
	}

	snapshotURL := "https://aur.archlinux.org" + targetPkg.URLPath
	archiveName := filepath.Base(targetPkg.URLPath)

	fmt.Printf("[pbb] Downloading AUR snapshot: %s\n", snapshotURL)

	tmpFile, tmpDir, err := downloadToTempDir(snapshotURL, archiveName)
	if err != nil {
		return fmt.Errorf("failed to download AUR snapshot: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("[pbb] Extracting snapshot...\n")
	if err := extractTarGz(tmpFile, "/tmp"); err != nil {
		return fmt.Errorf("failed to extract AUR snapshot: %v", err)
	}

	fmt.Printf("[+] Snapshot ready at: %s\n", filepath.Join("/tmp", targetPkg.Name))
	return nil
}

func extractTarGz(archivePath, targetDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("failed to open archive %s: %v", archivePath, err)
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to initialise gzip reader: %v", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading archive: %v", err)
		}

		target := filepath.Join(targetDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("failed to create parent dirs for %s: %v", target, err)
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("failed to create file %s: %v", target, err)
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to write file %s: %v", target, err)
			}
			outFile.Close()
		}
	}
	return nil
}

func parseAurDependencies(buildDir string) ([]string, error) {
	pkgbuildPath := filepath.Join(buildDir, "PKGBUILD")
	if _, err := os.Stat(pkgbuildPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("PKGBUILD not found in %s", buildDir)
	}

	// We source the PKGBUILD in bash to correctly expand arrays, variables, etc.
	// PKGBUILD is bash by spec — there is no portable alternative.
	script := fmt.Sprintf("source %s && echo ${depends[@]} ${makedepends[@]}", pkgbuildPath)
	cmd := exec.Command("bash", "-c", script)

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if _, ok := err.(*exec.ExitError); ok {
			exitErr = err.(*exec.ExitError)
			return nil, fmt.Errorf("failed to source PKGBUILD (exit %d): %s", exitErr.ExitCode(), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("failed to source PKGBUILD: %v", err)
	}

	rawDeps := strings.Fields(string(out))
	var cleanDeps []string
	for _, dep := range rawDeps {
		dep = strings.Trim(dep, "\"'")
		if dep != "" {
			cleanDeps = append(cleanDeps, dep)
		}
	}
	return cleanDeps, nil
}

func runAurBuildAndPackage(buildDir, pkgDir string) error {
	srcDir := filepath.Join(buildDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		return fmt.Errorf("failed to create srcDir: %v", err)
	}

	// Pass -x in verbose mode so each build command is echoed to stderr
	bashFlags := ""
	if verbose {
		bashFlags = "set -x\n"
	}

	script := fmt.Sprintf(`
		set -e
		%s
		cd "%s"

		source PKGBUILD

		export pkgdir="%s"
		export srcdir="%s"
		export PREFIX="/usr"
		export DESTDIR="%s"

		# Redirect python setup.py installs into pkgdir for isolation
		python() {
			if [[ "$*" == *"setup.py install"* ]]; then
				/usr/bin/python "$@" --root="$pkgdir"
			else
				/usr/bin/python "$@"
			fi
		}
		export -f python

		echo "[pbb-build] Fetching sources..."
		mkdir -p "$srcdir"

		for src_item in "${source[@]}"; do
			clean_url=$(echo "$src_item" | sed 's/.*:://')
			if [[ "$clean_url" == git+* || "$clean_url" == *.git ]]; then
				repo_url=$(echo "$clean_url" | sed 's/^git+//' | sed 's/#.*//')
				repo_name=$(basename "$repo_url" .git)
				echo "[pbb-build] Cloning: $repo_url"
				if [ ! -d "$srcdir/$repo_name" ]; then
					git clone "$repo_url" "$srcdir/$repo_name"
				fi
			elif [[ "$clean_url" == http://* || "$clean_url" == https://* ]]; then
				filename=$(basename "$clean_url")
				echo "[pbb-build] Downloading: $filename"
				if [ ! -f "$srcdir/$filename" ]; then
					curl -fL "$clean_url" -o "$srcdir/$filename" || {
						echo "[pbb-build] ERROR: failed to download $filename"
						exit 1
					}
				fi
				echo "[pbb-build] Extracting: $filename"
				if [[ "$filename" == *.tar.gz || "$filename" == *.tgz ]]; then
					tar -xf "$srcdir/$filename" -C "$srcdir"
				elif [[ "$filename" == *.tar.zst ]]; then
					tar -I zstd -xf "$srcdir/$filename" -C "$srcdir"
				elif [[ "$filename" == *.zip ]]; then
					unzip -q -o "$srcdir/$filename" -d "$srcdir"
				fi
			fi
		done

		cd "$srcdir"
		echo "[pbb-build] Running build()..."
		if declare -f build > /dev/null; then
			build || { echo "[pbb-build] ERROR: build() failed"; exit 1; }
		else
			echo "[pbb-build] No build() function, skipping."
		fi

		cd "$srcdir"
		echo "[pbb-build] Running package()..."
		if declare -f package > /dev/null; then
			package || { echo "[pbb-build] ERROR: package() failed"; exit 1; }
		else
			echo "[pbb] CRITICAL: package() function is missing in PKGBUILD!"
			exit 1
		fi

		echo "[pbb-build] Waiting for background jobs..."
		wait
		echo "[pbb-build] Build completed."
	`, bashFlags, buildDir, pkgDir, srcDir, pkgDir)

	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if _, ok := err.(*exec.ExitError); ok {
			exitErr = err.(*exec.ExitError)
			return fmt.Errorf("build script exited with code %d", exitErr.ExitCode())
		}
		return fmt.Errorf("build script failed: %v", err)
	}
	return nil
}

// deployBuiltFiles copies compiled files from srcDir into destDir, creates
// binary symlinks in binSymlinkDir, and links usr/share subdirectories.
// binSymlinkDir is passed explicitly so AUR packages use AurBinSymlinkDir.
func deployBuiltFiles(srcDir, destDir, binSymlinkDir string) ([]string, error) {
	var manifest []string

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("error accessing %s: %v", path, err)
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %v", path, err)
		}
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(destDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("failed to create parent dirs for %s: %v", targetPath, err)
		}

		// Explicit close — not deferred, to avoid fd leak inside Walk callback
		if err := copyFile(path, targetPath, info.Mode()); err != nil {
			return err
		}

		manifest = append(manifest, targetPath)

		// Create binary wrappers/symlinks for executables under usr/bin/
		if !strings.HasPrefix(relPath, "usr/bin/") {
			return nil
		}
		parts := strings.Split(relPath, "/")
		if len(parts) < 3 {
			return nil
		}
		binName := parts[2]

		if err := os.MkdirAll(binSymlinkDir, 0755); err != nil {
			return fmt.Errorf("failed to create binSymlinkDir %s: %v", binSymlinkDir, err)
		}

		wrapperPath := filepath.Join(binSymlinkDir, binName)
		realTarget := filepath.Join(destDir, "usr", "bin", binName)

		targetInfo, err := os.Stat(realTarget)
		if err != nil {
			// File not yet flushed or not a binary — skip silently
			return nil
		}

		_ = os.Remove(wrapperPath)

		if targetInfo.IsDir() {
			// Python package directory: locate the real entry point
			var execTarget string
			sameNameExec := filepath.Join(realTarget, binName)
			mainPyExec := filepath.Join(realTarget, "__main__.py")

			if fi, err := os.Stat(sameNameExec); err == nil && !fi.IsDir() {
				execTarget = sameNameExec
			} else if fi, err := os.Stat(mainPyExec); err == nil && !fi.IsDir() {
				execTarget = mainPyExec
			} else {
				return nil
			}

			wrapperContent := fmt.Sprintf("#!/bin/sh\nexport PYTHONWARNINGS=ignore\nexec python3 \"%s\" \"$@\"\n", execTarget)
			if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err != nil {
				if verbose {
					fmt.Printf("[v] Failed to write wrapper script %s: %v\n", wrapperPath, err)
				}
			} else {
				fmt.Printf("[pbb-deploy] Wrapper: %s -> %s\n", wrapperPath, execTarget)
				manifest = append(manifest, wrapperPath)
			}
		} else {
			if err := os.Symlink(realTarget, wrapperPath); err != nil {
				if verbose {
					fmt.Printf("[v] Failed to create symlink %s -> %s: %v\n", wrapperPath, realTarget, err)
				}
			} else {
				fmt.Printf("[pbb-deploy] Symlink: %s -> %s\n", wrapperPath, realTarget)
				manifest = append(manifest, wrapperPath)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Link usr/share subdirectories into ~/.local/share, skipping system-managed ones
	usrSharePath := filepath.Join(srcDir, "usr", "share")
	if entries, err := os.ReadDir(usrSharePath); err == nil {
		userShareDir := filepath.Join(os.Getenv("HOME"), ".local", "share")
		if err := os.MkdirAll(userShareDir, 0755); err != nil && verbose {
			fmt.Printf("[v] Failed to create user share dir: %v\n", err)
		}

		skipDirs := map[string]bool{
			"licenses": true, "man": true, "doc": true, "info": true,
			"bash-completion": true, "fish": true, "zsh": true, "applications": true,
		}

		for _, entry := range entries {
			if skipDirs[entry.Name()] {
				continue
			}

			isolatedDataDir := filepath.Join(destDir, "usr", "share", entry.Name())
			userDataSymlink := filepath.Join(userShareDir, entry.Name())

			_ = os.Remove(userDataSymlink)
			if err := os.Symlink(isolatedDataDir, userDataSymlink); err != nil {
				if verbose {
					fmt.Printf("[v] Failed to create share symlink %s: %v\n", userDataSymlink, err)
				}
			} else {
				fmt.Printf("[pbb-deploy] Share: %s -> %s\n", userDataSymlink, isolatedDataDir)
				manifest = append(manifest, userDataSymlink)
			}
		}
	}

	return manifest, nil
}

// copyFile copies src to dst with the given mode.
// Uses explicit Close calls — safe to call inside filepath.Walk.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %v", src, err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		in.Close()
		return fmt.Errorf("failed to create destination file %s: %v", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		in.Close()
		return fmt.Errorf("failed to copy %s -> %s: %v", src, dst, err)
	}

	if err := out.Close(); err != nil {
		in.Close()
		return fmt.Errorf("failed to flush %s: %v", dst, err)
	}
	in.Close()
	return nil
}

func printHelp() {
	fmt.Println("Usage: pbb <command> [flags] [packages]")
	fmt.Println("\nCommands:")
	fmt.Println("  -Syu                  Sync repository databases")
	fmt.Println("  -S  <pkg>             Install package from official repos")
	fmt.Println("  -R  <pkg>             Remove package")
	fmt.Println("  -Q  [pkg]             List installed packages")
	fmt.Println("  -q  <term>            Search in official repos")
	fmt.Println("  -AUR <term>           Search in official repos + AUR")
	fmt.Println("  -S-AUR <pkg>          Build and install AUR package")
	fmt.Println("  -start-service <name> Start a registered cogovinit service")
	fmt.Println("  -stop-service  <name> Stop a running cogovinit service")
	fmt.Println("\nFlags:")
	fmt.Println("  --bleeding            Use live Arch mirror instead of stable snapshot")
	fmt.Println("  --rollback            Re-download package from stable snapshot (verifies checksum)")
	fmt.Println("  -v                    Verbose output (includes patchelf errors, bash -x on builds)")
	fmt.Println("\nAUR isolation:")
	fmt.Println("  Files:    ~/.local/share/pbb/aur-root/")
	fmt.Println("  Binaries: ~/.local/bin/aur/  (add to PATH to use)")
}
