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
					if line == "%FILENAME%" {
						tempFilename = lines[i+1]
					}
					if line == "%VERSION%" {
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
						if line == "%NAME%" {
							pkgName = lines[i+1]
						}
						if line == "%VERSION%" {
							pkgVersion = lines[i+1]
						}
						if line == "%DESC%" {
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
		return fmt.Errorf("failed to parse AUR JSON payload: %v", err)
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

func createBinSymlink(targetPath string) string {
	fileName := filepath.Base(targetPath)
	if strings.HasPrefix(fileName, ".") || strings.Contains(targetPath, "/usr/lib/") {
		return ""
	}

	symlinkPath := filepath.Join(BinSymlinkDir, fileName)
	_ = os.Remove(symlinkPath)
	if err := os.Symlink(targetPath, symlinkPath); err == nil {
		return symlinkPath
	}
	return ""
}

func extractZstTar(archivePath, targetDir string) ([]string, error) {
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
		if strings.HasPrefix(header.Name, ".") {
			continue
		}

		target := filepath.Join(targetDir, header.Name)
		fileList = append(fileList, target)

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				return nil, err
			}
			io.Copy(outFile, tarReader)
			outFile.Close()

			isExecutable := (header.FileInfo().Mode() & 0111) != 0
			isSharedLib := strings.Contains(target, ".so")

			if isExecutable || isSharedLib {
				patchBinaryElf(target)
			}
			if isExecutable && (strings.Contains(target, "/bin/") || strings.Contains(target, "/sbin/")) {
				if sym := createBinSymlink(target); sym != "" {
					fileList = append(fileList, sym)
				}
			}

		case tar.TypeSymlink:
			os.Remove(target)
			linkName := header.Linkname
			if strings.HasPrefix(linkName, "/") {
				linkName = filepath.Join(targetDir, linkName)
			}
			os.Symlink(linkName, target)
		}
	}
	return fileList, nil
}

func removePackageWithManifest(pkgName string) error {
	manifestPath := filepath.Join(ManifestsDir, pkgName+".list")
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("file manifest not found")
	}

	files := strings.Split(string(data), "\n")
	for i := len(files) - 1; i >= 0; i-- {
		file := strings.TrimSpace(files[i])
		if file == "" || file == "/" {
			continue
		}
		fi, err := os.Stat(file)
		if err != nil {
			os.Remove(file)
			continue
		}
		if fi.IsDir() {
			os.Remove(file)
		} else {
			os.Remove(file)
		}
	}
	os.Remove(manifestPath)
	unregisterPackage(pkgName)
	fmt.Printf("[+] Package '%s' successfully removed from the user environment.\n", pkgName)
	return nil
}

// --- System Directories & Meta ---

func initSystemDirs() error {
	paths := []string{PbbDir, ManifestsDir, SyncDir, TargetPrefix, BinSymlinkDir}
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

func handleRollback(pkgName string, stableMirror string) {
	db, err := readLocalDb()
	if err != nil {
		return
	}
	pkg, exists := db[pkgName]
	if !exists || pkg.Branch != "bleeding" {
		return
	}
	pkg.Branch = "stable"
	db[pkgName] = pkg
	writeLocalDb(db)
}

func readLocalDb() (map[string]PackageInfo, error) {
	file, err := os.ReadFile(LocalDbPath)
	if err != nil {
		return nil, err
	}
	var db map[string]PackageInfo
	json.Unmarshal(file, &db)
	return db, nil
}

func writeLocalDb(db map[string]PackageInfo) error {
	data, _ := json.MarshalIndent(db, "", "  ")
	return os.WriteFile(LocalDbPath, data, 0644)
}

func registerPackage(name, version, branch string) error {
	db, _ := readLocalDb()
	db[name] = PackageInfo{Name: name, Version: version, Branch: branch}
	return writeLocalDb(db)
}

func unregisterPackage(name string) error {
	db, _ := readLocalDb()
	delete(db, name)
	return writeLocalDb(db)
}

func printHelp() {
	fmt.Println("Usage: pbb <command> [flags] [packages]")
	fmt.Println("\nCommands:")
	fmt.Println("  -Syu                      Synchronize repository databases")
	fmt.Println("  -S <package_name>         Install package")
	fmt.Println("  -R <package_name>         Remove package")
	fmt.Println("  -Q [package_name]         List installed packages")
	fmt.Println("  -q <search_string>        Search package in official repositories")
	fmt.Println("  -AUR <search_string>      Global search across official databases + AUR")
	fmt.Println("  -S-AUR <package_name>     Download and extract AUR snapshot for compilation")
}

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
		return fmt.Errorf("failed to parse endpoint response: %v", err)
	}

	if aurData.ResultCount == 0 {
		return fmt.Errorf("package '%s' not found in AUR database", pkgName)
	}

	targetPkg := aurData.Results[0]
	if targetPkg.URLPath == "" {
		return fmt.Errorf("AUR did not provide a snapshot link for %s", pkgName)
	}

	snapshotURL := "https://aur.archlinux.org" + targetPkg.URLPath
	archiveName := filepath.Base(targetPkg.URLPath)
	tmpArchivePath := filepath.Join("/tmp", archiveName)

	fmt.Printf("[pbb] Downloading snapshot from AUR: %s\n", snapshotURL)
	if err := downloadFile(snapshotURL, tmpArchivePath); err != nil {
		return fmt.Errorf("failed to download archive: %v", err)
	}
	defer os.Remove(tmpArchivePath)

	fmt.Printf("[pbb] Extracting snapshot to /tmp...\n")
	if err := extractTarGz(tmpArchivePath, "/tmp"); err != nil {
		return fmt.Errorf("failed to extract archive: %v", err)
	}

	buildDir := filepath.Join("/tmp", targetPkg.Name)
	fmt.Printf("[+] Snapshot successfully prepared at: %s\n", buildDir)
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
		return nil, fmt.Errorf("PKGBUILD file is missing in directory %s", buildDir)
	}

	script := fmt.Sprintf("source %s && echo ${depends[@]} ${makedepends[@]}", pkgbuildPath)
	cmd := exec.Command("bash", "-c", script)
	
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute Bash script: %v", err)
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

	// Build script executing execution phase switches into $srcdir before build and package operations
	script := fmt.Sprintf(`
		cd "%s"
		
		# Load PKGBUILD context into bash environment
		source PKGBUILD
		
		export pkgdir="%s"
		export srcdir="%s"
		
		# Set standard compilation variables
		export PREFIX="/usr"
		export DESTDIR="%s"

		# Intercept python setup.py executions to ensure isolation
		python() {
			if [[ "$*" == *"setup.py install"* ]]; then
				/usr/bin/python "$@" --root="$pkgdir"
			else
				/usr/bin/python "$@"
			fi
		}
		export -f python

		echo "[pbb-build] Processing sources..."
		mkdir -p "$srcdir"
		
		for src_item in "${source[@]}"; do
			clean_url=$(echo "$src_item" | sed 's/.*:://')
			if [[ "$clean_url" == git+* || "$clean_url" == *.git ]]; then
				repo_url=$(echo "$clean_url" | sed 's/^git+//' | sed 's/#.*//')
				repo_name=$(basename "$repo_url" .git)
				echo "[pbb-build] Cloning git source: $repo_url"
				if [ ! -d "$srcdir/$repo_name" ]; then
					git clone "$repo_url" "$srcdir/$repo_name"
				fi
			elif [[ "$clean_url" == http://* || "$clean_url" == https://* ]]; then
				filename=$(basename "$clean_url")
				echo "[pbb-build] Downloading archive: $filename"
				if [ ! -f "$srcdir/$filename" ]; then
					curl -L "$clean_url" -o "$srcdir/$filename"
				fi
				echo "[pbb-build] Extracting $filename..."
				if [[ "$filename" == *.tar.gz || "$filename" == *.tgz ]]; then
					tar -xf "$srcdir/$filename" -C "$srcdir"
				elif [[ "$filename" == *.tar.zst ]]; then
					tar -I zstd -xf "$srcdir/$filename" -C "$srcdir"
				elif [[ "$filename" == *.zip ]]; then
					unzip -q -o "$srcdir/$filename" -D "$srcdir"
				fi
			fi
		done

		# Change directory into source directory for compilation stage
		cd "$srcdir"
		echo "[build] Running build function..."
		if declare -f build > /dev/null; then
			build
		else
			echo "[build] build() function missing, skipping."
		fi

		# Reset workspace context to source directory for packaging stage
		cd "$srcdir"
		echo "[package] Running package function..."
		if declare -f package > /dev/null; then
			package
		else
			echo "[pbb] CRITICAL ERROR: package() function is missing!"
			exit 1
		fi

		echo "[pbb] Waiting for background compilation utilities to exit..."
		while pgrep -P $$ > /dev/null; do
			sleep 0.2
		done
		echo "[pbb] Compilation sequence completed successfully."
	`, buildDir, pkgDir, srcDir, pkgDir)

	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// deployBuiltFiles transfers compiled structures into target directories and builds a file manifest
func deployBuiltFiles(srcDir, destDir string) ([]string, error) {
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
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return err
			}
		} else {
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

			// --- Setup environment binaries and launchers inside usr/bin ---
			if strings.HasPrefix(relPath, "usr/bin/") {
				parts := strings.Split(relPath, "/")
				if len(parts) >= 3 {
					binName := parts[2]

					_ = os.MkdirAll(BinSymlinkDir, 0755)
					wrapperPath := filepath.Join(BinSymlinkDir, binName)

					realTarget := filepath.Join(destDir, "usr", "bin", binName)
					targetInfo, err := os.Stat(realTarget)

					if err == nil {
						_ = os.Remove(wrapperPath)

						if targetInfo.IsDir() {
							// Wrapper generation for packages containing script directories
							var executableTarget string
							sameNameExec := filepath.Join(realTarget, binName)
							mainPyExec := filepath.Join(realTarget, "__main__.py")

							if info, err := os.Stat(sameNameExec); err == nil && !info.IsDir() {
								executableTarget = sameNameExec
							} else if info, err := os.Stat(mainPyExec); err == nil && !info.IsDir() {
								executableTarget = mainPyExec
							} else {
								return nil 
							}

							wrapperContent := fmt.Sprintf("#!/bin/sh\nexport PYTHONWARNINGS=ignore\nexec python3 \"%s\" \"$@\"\n", executableTarget)
							if err := os.WriteFile(wrapperPath, []byte(wrapperContent), 0755); err == nil {
								fmt.Printf("[pbb-deploy] Created directory wrapper script: %s -> %s\n", wrapperPath, executableTarget)
								manifest = append(manifest, wrapperPath)
							}
						} else {
							if err := os.Symlink(realTarget, wrapperPath); err == nil {
								fmt.Printf("[pbb-deploy] Created binary symlink: %s -> %s\n", wrapperPath, realTarget)
								manifest = append(manifest, wrapperPath)
							}
						}
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	// --- Automate shared structures linking (usr/share) ---
	usrSharePath := filepath.Join(srcDir, "usr", "share")
	if entries, err := os.ReadDir(usrSharePath); err == nil {
		userShareDir := filepath.Join(os.Getenv("HOME"), ".local", "share")
		_ = os.MkdirAll(userShareDir, 0755)

		for _, entry := range entries {
			systemFolders := map[string]bool{
				"licenses": true, "man": true, "doc": true, "info": true,
				"bash-completion": true, "fish": true, "zsh": true, "applications": true,
			}
			if systemFolders[entry.Name()] {
				continue
			}

			isolatedDataDir := filepath.Join(destDir, "usr", "share", entry.Name())
			userDataSymlink := filepath.Join(userShareDir, entry.Name())

			_ = os.Remove(userDataSymlink)

			if err := os.Symlink(isolatedDataDir, userDataSymlink); err == nil {
				fmt.Printf("[pbb-deploy] Integrated assets directory: %s -> %s\n", userDataSymlink, isolatedDataDir)
				manifest = append(manifest, userDataSymlink)
			}
		}
	}

	return manifest, nil
}
