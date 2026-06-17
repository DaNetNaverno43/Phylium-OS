package main

import (
	"archive/tar"
	"compress/gzip"
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
// The filename format is: <name>-<pkgver>-<pkgrel>-<arch>.pkg.tar.zst
// We strip from the end: arch, pkgrel, pkgver — what remains is the name.
// This correctly handles names with hyphens like "lib32-mesa" or "python-requests".
func parsePkgNameFromFilename(filename string) string {
	// Strip known suffixes
	name := strings.TrimSuffix(filename, ".pkg.tar.zst")
	name = strings.TrimSuffix(name, ".pkg.tar.xz")

	parts := strings.Split(name, "-")
	// Minimum valid: name-pkgver-pkgrel-arch → at least 4 parts
	// Strip the last 3 (arch, pkgrel, pkgver) to get the package name.
	if len(parts) >= 4 {
		return strings.Join(parts[:len(parts)-3], "-")
	}
	// Fallback: just return the first segment
	return parts[0]
}

// --- Arch Repository Database Parser ---

func searchPackageInRepositories(targetPkg string) (repoName string, filename string, version string, err error) {
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
			return "", "", "", err
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
				return "", "", "", err
			}

			parts := strings.Split(header.Name, "/")
			if len(parts) < 2 {
				continue
			}

			// Directory name format in .db is "<name>-<pkgver>-<pkgrel>".
			// We need to extract just the package name (strip last two dash-separated segments).
			fullDirName := parts[0]
			var dashIndices []int
			for i := 0; i < len(fullDirName); i++ {
				if fullDirName[i] == '-' {
					dashIndices = append(dashIndices, i)
				}
			}

			var currentPkgName string
			if len(dashIndices) >= 2 {
				currentPkgName = fullDirName[:dashIndices[len(dashIndices)-2]]
			} else {
				currentPkgName = strings.Split(fullDirName, "-")[0]
			}

			if parts[1] == "desc" && currentPkgName == targetPkg {
				buf := new(strings.Builder)
				if _, err := io.Copy(buf, tarReader); err != nil {
					gzipReader.Close()
					file.Close()
					return "", "", "", err
				}

				lines := strings.Split(buf.String(), "\n")
				var tempFilename, tempVersion string
				for i, line := range lines {
					if line == "%FILENAME%" && i+1 < len(lines) {
						tempFilename = lines[i+1]
					}
					if line == "%VERSION%" && i+1 < len(lines) {
						tempVersion = lines[i+1]
					}
				}

				if tempFilename != "" && tempVersion != "" {
					gzipReader.Close()
					file.Close()
					return repo, tempFilename, tempVersion, nil
				}
			}
		}
		gzipReader.Close()
		file.Close()
	}

	return "", "", "", fmt.Errorf("package '%s' not found in any repository", targetPkg)
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
			return err
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
				return err
			}

			parts := strings.Split(header.Name, "/")
			if len(parts) < 2 {
				continue
			}

			fullDirName := parts[0]

			if strings.Contains(strings.ToLower(fullDirName), strings.ToLower(searchTerm)) {
				if parts[1] == "desc" && !parsedPackages[fullDirName] {
					parsedPackages[fullDirName] = true

					buf := new(strings.Builder)
					if _, err := io.Copy(buf, tarReader); err != nil {
						continue
					}

					lines := strings.Split(buf.String(), "\n")
					var pkgName, pkgVersion, pkgDesc string

					for i, line := range lines {
						if line == "%NAME%" && i+1 < len(lines) {
							pkgName = lines[i+1]
						}
						if line == "%VERSION%" && i+1 < len(lines) {
							pkgVersion = lines[i+1]
						}
						if line == "%DESC%" && i+1 < len(lines) {
							pkgDesc = lines[i+1]
						}
					}

					if pkgName != "" {
						fmt.Printf("\033[1;34m%s/\033[1;32m%s \033[1;37m%s\033[0m\n", repo, pkgName, pkgVersion)
						if pkgDesc != "" {
							fmt.Printf("    %s\n", pkgDesc)
						}
						foundCount++
					}
				}
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

// --- AUR Extension Functions ---

func searchAur(searchTerm string) error {
	url := fmt.Sprintf("https://aur.archlinux.org/rpc/?v=5&type=search&arg=%s", searchTerm)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
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

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pbb-Package-Manager/1.0 (RootlessOS; SysAdmin Custom Tool)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned non-200 status: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func patchBinaryElf(targetPath string) {
	rpath := "$ORIGIN/../lib:$ORIGIN/../../usr/lib:$ORIGIN/../../lib:$ORIGIN/../usr/lib"
	cmd := exec.Command("patchelf", "--set-rpath", rpath, targetPath)
	_ = cmd.Run()
}

// createBinSymlink creates a symlink from symlinkDir/basename -> targetPath.
// symlinkDir is passed explicitly so repo and AUR packages use separate dirs.
func createBinSymlink(targetPath, symlinkDir string) string {
	fileName := filepath.Base(targetPath)
	if strings.HasPrefix(fileName, ".") || strings.Contains(targetPath, "/usr/lib/") {
		return ""
	}

	if err := os.MkdirAll(symlinkDir, 0755); err != nil {
		return ""
	}

	symlinkPath := filepath.Join(symlinkDir, fileName)
	_ = os.Remove(symlinkPath)
	if err := os.Symlink(targetPath, symlinkPath); err == nil {
		return symlinkPath
	}
	return ""
}

// extractZstTar extracts a .pkg.tar.zst archive into targetDir.
// Binaries found in /bin/ or /sbin/ get patchelf treatment and a symlink in symlinkDir.
func extractZstTar(archivePath, targetDir, symlinkDir string) ([]string, error) {
	var fileList []string
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	zstdReader, err := zstd.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer zstdReader.Close()

	tarReader := tar.NewReader(zstdReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Skip package metadata entries (start with ".")
		if strings.HasPrefix(header.Name, ".") {
			continue
		}

		target := filepath.Join(targetDir, header.Name)
		fileList = append(fileList, target)

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return nil, err
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
			os.Remove(target)
			linkName := header.Linkname
			// Rewrite absolute symlinks to be relative to the targetDir prefix
			if strings.HasPrefix(linkName, "/") {
				linkName = filepath.Join(targetDir, linkName)
			}
			os.Symlink(linkName, target)
		}
	}
	return fileList, nil
}

// removePackageWithManifest removes all files recorded in a package's manifest,
// then cleans up empty parent directories, the manifest itself, and the db entry.
func removePackageWithManifest(pkgName string) error {
	manifestPath := filepath.Join(ManifestsDir, pkgName+".list")
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("manifest not found for '%s' — is it installed?", pkgName)
	}
	if err != nil {
		return fmt.Errorf("failed to read manifest: %v", err)
	}

	files := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Remove in reverse order so files are deleted before their parent directories
	for i := len(files) - 1; i >= 0; i-- {
		f := strings.TrimSpace(files[i])
		if f == "" || f == "/" {
			continue
		}

		fi, err := os.Lstat(f) // Lstat so we handle symlinks correctly
		if err != nil {
			// Already gone — not an error, keep going
			continue
		}

		if fi.IsDir() {
			// Only remove the directory if it's empty; if other packages put files
			// there we must not touch it
			if err := os.Remove(f); err != nil && verbose {
				fmt.Printf("[v] Skipping non-empty directory: %s\n", f)
			}
		} else {
			if err := os.Remove(f); err != nil && verbose {
				fmt.Printf("[v] Failed to remove file: %s: %v\n", f, err)
			}
		}
	}

	os.Remove(manifestPath)
	unregisterPackage(pkgName)
	fmt.Printf("[+] Package '%s' removed.\n", pkgName)
	return nil
}

// --- System Directories & State ---

func initSystemDirs() error {
	paths := []string{PbbDir, ManifestsDir, SyncDir, TargetPrefix, AurTargetPrefix, BinSymlinkDir, AurBinSymlinkDir}
	for _, p := range paths {
		if err := os.MkdirAll(p, 0755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(LocalDbPath); os.IsNotExist(err) {
		return os.WriteFile(LocalDbPath, []byte("{}"), 0644)
	}
	return nil
}

func saveManifest(pkgName string, files []string) error {
	content := strings.Join(files, "\n")
	return os.WriteFile(filepath.Join(ManifestsDir, pkgName+".list"), []byte(content), 0644)
}

func GetOrUpdateSnapshot() (string, error) {
	now := time.Now()
	file, err := os.ReadFile(StateFilePath)
	if os.IsNotExist(err) {
		initialDate := now.AddDate(0, 0, -14)
		state := PbbState{CurrentSnapshot: initialDate.Format("2006/01/02"), LastUpdated: now}
		return state.CurrentSnapshot, saveState(state)
	} else if err != nil {
		return "", err
	}

	var state PbbState
	json.Unmarshal(file, &state)

	if now.Sub(state.LastUpdated) >= SnapshotDuration {
		parsedTime, _ := time.Parse("2006/01/02", state.CurrentSnapshot)
		state.CurrentSnapshot = parsedTime.AddDate(0, 0, 14).Format("2006/01/02")
		state.LastUpdated = now
		return state.CurrentSnapshot, saveState(state)
	}
	return state.CurrentSnapshot, nil
}

func saveState(state PbbState) error {
	data, _ := json.MarshalIndent(state, "", "  ")
	return os.WriteFile(StateFilePath, data, 0644)
}

// handleRollback re-downloads the package from the stable archive mirror,
// replacing whatever bleeding-edge version is currently installed.
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
		fmt.Printf("[pbb] Package '%s' is already on stable branch. Nothing to do.\n", pkgName)
		return
	}

	fmt.Printf("[pbb] Rolling back '%s' from bleeding to stable...\n", pkgName)

	// Find the stable version in the repo database
	repoType, pkgFilename, pkgVersion, err := searchPackageInRepositories(pkgName)
	if err != nil {
		fmt.Printf("[pbb] Could not find '%s' in stable repositories: %v\n", pkgName, err)
		return
	}

	url := fmt.Sprintf("%s/%s/os/x86_64/%s", stableMirror, repoType, pkgFilename)
	tmpFile := filepath.Join("/tmp", pkgFilename)

	if err := downloadFile(url, tmpFile); err != nil {
		fmt.Printf("[pbb] Failed to download stable version of '%s': %v\n", pkgName, err)
		return
	}
	defer os.Remove(tmpFile)

	// Determine which prefix/symlink dir this package belongs to
	targetPrefix := TargetPrefix
	symlinkDir := BinSymlinkDir
	if pkg.Source == "aur" {
		targetPrefix = AurTargetPrefix
		symlinkDir = AurBinSymlinkDir
	}

	// Remove old files before extracting the stable version
	if err := removePackageWithManifest(pkgName); err != nil && verbose {
		fmt.Printf("[v] Pre-rollback cleanup warning: %v\n", err)
	}

	manifest, err := extractZstTar(tmpFile, targetPrefix, symlinkDir)
	if err != nil {
		fmt.Printf("[pbb] Failed to extract stable package: %v\n", err)
		return
	}

	if err := saveManifest(pkgName, manifest); err != nil {
		fmt.Printf("[pbb] Failed to save manifest after rollback: %v\n", err)
	}

	registerPackage(pkgName, pkgVersion, "stable", pkg.Source)
	fmt.Printf("[+] Package '%s' successfully rolled back to stable (%s).\n", pkgName, pkgVersion)
}

// --- Local Database ---

func readLocalDb() (map[string]PackageInfo, error) {
	file, err := os.ReadFile(LocalDbPath)
	if err != nil {
		return nil, err
	}
	var db map[string]PackageInfo
	json.Unmarshal(file, &db)
	if db == nil {
		db = make(map[string]PackageInfo)
	}
	return db, nil
}

func writeLocalDb(db map[string]PackageInfo) error {
	data, _ := json.MarshalIndent(db, "", "  ")
	return os.WriteFile(LocalDbPath, data, 0644)
}

func registerPackage(name, version, branch, source string) error {
	db, _ := readLocalDb()
	db[name] = PackageInfo{Name: name, Version: version, Branch: branch, Source: source}
	return writeLocalDb(db)
}

func unregisterPackage(name string) error {
	db, _ := readLocalDb()
	delete(db, name)
	return writeLocalDb(db)
}

// --- AUR Build Pipeline ---

func downloadAndExtractAurSnapshot(pkgName string) error {
	url := fmt.Sprintf("https://aur.archlinux.org/rpc/?v=5&type=info&arg[]=%s", pkgName)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pbb-Package-Manager/1.0 (RootlessOS; AUR Fetcher)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AUR RPC network error: %v", err)
	}
	defer resp.Body.Close()

	var aurData AurResponse
	if err := json.NewDecoder(resp.Body).Decode(&aurData); err != nil {
		return fmt.Errorf("failed to parse AUR response: %v", err)
	}

	if aurData.ResultCount == 0 {
		return fmt.Errorf("package '%s' not found in AUR", pkgName)
	}

	targetPkg := aurData.Results[0]
	if targetPkg.URLPath == "" {
		return fmt.Errorf("AUR did not return a snapshot URL for '%s'", pkgName)
	}

	snapshotURL := "https://aur.archlinux.org" + targetPkg.URLPath
	archiveName := filepath.Base(targetPkg.URLPath)
	tmpArchivePath := filepath.Join("/tmp", archiveName)

	fmt.Printf("[pbb] Downloading AUR snapshot: %s\n", snapshotURL)
	if err := downloadFile(snapshotURL, tmpArchivePath); err != nil {
		return fmt.Errorf("failed to download snapshot: %v", err)
	}
	defer os.Remove(tmpArchivePath)

	fmt.Printf("[pbb] Extracting snapshot...\n")
	if err := extractTarGz(tmpArchivePath, "/tmp"); err != nil {
		return fmt.Errorf("failed to extract snapshot: %v", err)
	}

	fmt.Printf("[+] Snapshot ready at: %s\n", filepath.Join("/tmp", targetPkg.Name))
	return nil
}

func extractTarGz(archivePath, targetDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(targetDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
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

	// We source the PKGBUILD in bash to let it expand variables, arrays, etc.
	// This is intentional — PKGBUILD is bash by spec and there is no portable
	// alternative to evaluating it correctly without a bash interpreter.
	script := fmt.Sprintf("source %s && echo ${depends[@]} ${makedepends[@]}", pkgbuildPath)
	cmd := exec.Command("bash", "-c", script)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to source PKGBUILD: %v", err)
	}

	rawDeps := strings.Fields(string(output))
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
		return err
	}

	script := fmt.Sprintf(`
		set -e
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
					curl -L "$clean_url" -o "$srcdir/$filename"
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
			build
		else
			echo "[pbb-build] No build() function, skipping."
		fi

		cd "$srcdir"
		echo "[pbb-build] Running package()..."
		if declare -f package > /dev/null; then
			package
		else
			echo "[pbb] CRITICAL: package() function is missing in PKGBUILD!"
			exit 1
		fi

		echo "[pbb-build] Waiting for background jobs..."
		wait
		echo "[pbb-build] Build completed."
	`, buildDir, pkgDir, srcDir, pkgDir)

	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// deployBuiltFiles copies compiled files from srcDir into destDir, creates
// binary symlinks in binSymlinkDir, and handles usr/share linking.
// binSymlinkDir is passed explicitly so AUR packages use AurBinSymlinkDir.
func deployBuiltFiles(srcDir, destDir, binSymlinkDir string) ([]string, error) {
	var manifest []string

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(destDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		srcFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		destFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer destFile.Close()

		if _, err := io.Copy(destFile, srcFile); err != nil {
			return err
		}

		manifest = append(manifest, targetPath)

		// Create binary wrappers/symlinks for executables in usr/bin/
		if strings.HasPrefix(relPath, "usr/bin/") {
			parts := strings.Split(relPath, "/")
			if len(parts) < 3 {
				return nil
			}
			binName := parts[2]

			if err := os.MkdirAll(binSymlinkDir, 0755); err != nil {
				return err
			}
			wrapperPath := filepath.Join(binSymlinkDir, binName)
			realTarget := filepath.Join(destDir, "usr", "bin", binName)

			targetInfo, err := os.Stat(realTarget)
			if err != nil {
				return nil
			}

			_ = os.Remove(wrapperPath)

			if targetInfo.IsDir() {
				// Python package dir: find the actual entry point
				var executableTarget string
				sameNameExec := filepath.Join(realTarget, binName)
				mainPyExec := filepath.Join(realTarget, "__main__.py")

				if fi, err := os.Stat(sameNameExec); err == nil && !fi.IsDir() {
					executableTarget = sameNameExec
				} else if fi, err := os.Stat(mainPyExec); err == nil && !fi.IsDir() {
					executableTarget = mainPyExec
				} else {
					return nil
				}

				wrapperContent := fmt.Sprintf("#!/bin/sh\nexport PYTHONWARNINGS=ignore\nexec python3 \"%s\" \"$@\"\n", executableTarget)
				if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err == nil {
					fmt.Printf("[pbb-deploy] Wrapper script: %s -> %s\n", wrapperPath, executableTarget)
					manifest = append(manifest, wrapperPath)
				}
			} else {
				if err := os.Symlink(realTarget, wrapperPath); err == nil {
					fmt.Printf("[pbb-deploy] Binary symlink: %s -> %s\n", wrapperPath, realTarget)
					manifest = append(manifest, wrapperPath)
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Link usr/share subdirectories into ~/.local/share (skip system-managed ones)
	usrSharePath := filepath.Join(srcDir, "usr", "share")
	if entries, err := os.ReadDir(usrSharePath); err == nil {
		userShareDir := filepath.Join(os.Getenv("HOME"), ".local", "share")
		_ = os.MkdirAll(userShareDir, 0755)

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
			if err := os.Symlink(isolatedDataDir, userDataSymlink); err == nil {
				fmt.Printf("[pbb-deploy] Share symlink: %s -> %s\n", userDataSymlink, isolatedDataDir)
				manifest = append(manifest, userDataSymlink)
			}
		}
	}

	return manifest, nil
}

func printHelp() {
	fmt.Println("Usage: pbb <command> [flags] [packages]")
	fmt.Println("\nCommands:")
	fmt.Println("  -Syu                      Sync repository databases")
	fmt.Println("  -S  <pkg>                 Install package from official repos")
	fmt.Println("  -R  <pkg>                 Remove package")
	fmt.Println("  -Q  [pkg]                 List installed packages")
	fmt.Println("  -q  <term>                Search in official repos")
	fmt.Println("  -AUR <term>               Search in official repos + AUR")
	fmt.Println("  -S-AUR <pkg>              Build and install AUR package")
	fmt.Println("\nFlags (combine with commands above):")
	fmt.Println("  --bleeding                Use live Arch mirror instead of snapshot")
	fmt.Println("  --rollback                Re-download package from stable snapshot mirror")
	fmt.Println("  -v                        Verbose output")
	fmt.Println("\nAUR packages are isolated in:")
	fmt.Println("  Files:    ~/.local/share/pbb/aur-root/")
	fmt.Println("  Binaries: ~/.local/bin/aur/  (add to PATH to use)")
}
