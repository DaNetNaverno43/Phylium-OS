package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Global environment path components evaluated dynamically inside init()
var (
	BasePbbDir       string
	PbbDir           string
	StateFilePath    string
	LocalDbPath      string
	ManifestsDir     string
	SyncDir          string
	TargetPrefix     string // Root prefix for official repo packages (~/.local/share/pbb/root)
	AurTargetPrefix  string // Root prefix for AUR packages (~/.local/share/pbb/aur-root)
	BinSymlinkDir    string // Symlink dir for official repo binaries (~/.local/bin)
	AurBinSymlinkDir string // Symlink dir for AUR binaries (~/.local/bin/aur)
)

const (
	SnapshotDuration = 14 * 24 * time.Hour
	LiveArchMirror   = "https://mirror.yandex.ru/archlinux"
	ArchiveBaseURL   = "https://archive.archlinux.org/repos"
)

type PbbState struct {
	CurrentSnapshot string    `json:"current_snapshot"`
	LastUpdated     time.Time `json:"last_updated"`
}

// PackageInfo tracks an installed package. Source is "repo" or "aur".
type PackageInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Branch  string `json:"branch"`
	Source  string `json:"source"`
}

type AurPackage struct {
	Name        string `json:"Name"`
	Version     string `json:"Version"`
	Description string `json:"Description"`
	URLPath     string `json:"URLPath"`
}

type AurResponse struct {
	ResultCount int          `json:"resultcount"`
	Results     []AurPackage `json:"results"`
}

var verbose bool

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("[pbb] Critical error: failed to determine user home directory: %v\n", err)
		os.Exit(1)
	}

	BasePbbDir       = filepath.Join(home, ".local", "share", "pbb")
	PbbDir           = filepath.Join(BasePbbDir, "system")
	TargetPrefix     = filepath.Join(BasePbbDir, "root")
	AurTargetPrefix  = filepath.Join(BasePbbDir, "aur-root")
	BinSymlinkDir    = filepath.Join(home, ".local", "bin")
	AurBinSymlinkDir = filepath.Join(home, ".local", "bin", "aur")

	StateFilePath = filepath.Join(PbbDir, "state.json")
	LocalDbPath   = filepath.Join(PbbDir, "local_db.json")
	ManifestsDir  = filepath.Join(PbbDir, "manifests")
	SyncDir       = filepath.Join(PbbDir, "sync")
}

func main() {
	if _, err := exec.LookPath("patchelf"); err != nil {
		fmt.Println("[pbb] Error: executable utility 'patchelf' not found on the host system!")
		fmt.Println("       Please install it using your system package manager (e.g.: sudo xbps-install -S patchelf)")
		os.Exit(1)
	}

	if err := initSystemDirs(); err != nil {
		fmt.Printf("[pbb] Critical system directories initialization error: %v\n", err)
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	action := os.Args[1]

	var bleedingOpt bool
	var rollbackOpt bool
	var verboseOpt bool
	var packages []string

	fs := flag.NewFlagSet("pbb", flag.ExitOnError)
	fs.BoolVar(&bleedingOpt, "bleeding", false, "Enable tracking of bleeding edge branch components")
	fs.BoolVar(&rollbackOpt, "rollback", false, "Rollback package from bleeding edge to stable")
	fs.BoolVar(&verboseOpt, "v", false, "Enable verbose troubleshooting log output")
	fs.Parse(os.Args[2:])
	packages = fs.Args()
	verbose = verboseOpt

	stableSnapshot, err := GetOrUpdateSnapshot()
	if err != nil {
		fmt.Printf("[pbb] Failed to compute archive snapshot state: %v\n", err)
		os.Exit(1)
	}
	stableMirror := fmt.Sprintf("%s/%s", ArchiveBaseURL, stableSnapshot)

	currentMirror := stableMirror
	branchName := "stable"
	if bleedingOpt {
		currentMirror = LiveArchMirror
		branchName = "bleeding"
	}

	// --rollback can accompany any action; handle it first
	if rollbackOpt {
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing rollback target package name.")
			os.Exit(1)
		}
		for _, pkg := range packages {
			handleRollback(pkg, stableMirror)
		}
		return
	}

	switch action {
	case "-Syu":
		fmt.Println("[pbb] Synchronizing package repository databases...")
		repositories := []string{"core", "extra", "multilib"}

		for _, repo := range repositories {
			dbURL := fmt.Sprintf("%s/%s/os/x86_64/%s.db", currentMirror, repo, repo)
			localDbTarget := filepath.Join(SyncDir, repo+".db")

			if verbose {
				fmt.Printf("[v] Querying remote repository database index %s: %s\n", repo, dbURL)
			}

			if err := downloadFile(dbURL, localDbTarget); err != nil {
				fmt.Printf("[pbb] Error synchronizing repository database index %s: %v\n", repo, err)
				os.Exit(1)
			}
		}
		fmt.Println("[+] Repository database index snapshots updated successfully.")

	case "-S":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: No package targets specified.")
			os.Exit(1)
		}

		for _, pkgName := range packages {
			if strings.HasSuffix(pkgName, ".pkg.tar.zst") {
				pkgName = parsePkgNameFromFilename(pkgName)
			}

			fmt.Printf("[pbb] Searching for package '%s' in repository indexes...\n", pkgName)
			repoType, pkgFilename, pkgVersion, err := searchPackageInRepositories(pkgName)
			if err != nil {
				fmt.Printf("[pbb] Error matching target component: %v. Please execute pbb -Syu first\n", err)
				continue
			}

			if verbose {
				fmt.Printf("[v] Repository hit: %s | Filename: %s | Version: %s\n", repoType, pkgFilename, pkgVersion)
			}

			url := fmt.Sprintf("%s/%s/os/x86_64/%s", currentMirror, repoType, pkgFilename)
			fmt.Printf("[pbb] Downloading '%s' from [%s]...\n", pkgName, repoType)

			tmpFile := filepath.Join("/tmp", pkgFilename)
			if err := downloadFile(url, tmpFile); err != nil {
				fmt.Printf("[pbb] Download failed: %v\n", err)
				continue
			}

			if verbose {
				fmt.Printf("[v] Extracting %s into %s...\n", pkgFilename, TargetPrefix)
			}

			manifest, err := extractZstTar(tmpFile, TargetPrefix, BinSymlinkDir)
			if err != nil {
				fmt.Printf("[pbb] Extraction error: %v\n", err)
				os.Remove(tmpFile)
				continue
			}

			if err := saveManifest(pkgName, manifest); err != nil {
				fmt.Printf("[pbb] Failed to save manifest: %v\n", err)
			}

			registerPackage(pkgName, pkgVersion, branchName, "repo")
			os.Remove(tmpFile)
			fmt.Printf("[+] Package '%s' successfully installed [%s].\n", pkgName, branchName)
		}

	case "-R":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: No targets specified for removal.")
			os.Exit(1)
		}
		for _, pkg := range packages {
			cleanPkgName := pkg
			if strings.HasSuffix(cleanPkgName, ".pkg.tar.zst") {
				cleanPkgName = parsePkgNameFromFilename(cleanPkgName)
			}
			if err := removePackageWithManifest(cleanPkgName); err != nil {
				fmt.Printf("[pbb] Removal failed for '%s': %v\n", cleanPkgName, err)
			}
		}

	case "-Q":
		db, err := readLocalDb()
		if err != nil {
			fmt.Printf("[pbb] Failed to read local database: %v\n", err)
			os.Exit(1)
		}

		if len(packages) == 0 {
			fmt.Println("[pbb] Installed packages:")
			for name, info := range db {
				fmt.Printf("  %s %s [%s] (%s)\n", name, info.Version, info.Branch, info.Source)
			}
		} else {
			for _, pkgName := range packages {
				info, exists := db[pkgName]
				if exists {
					fmt.Printf("%s %s [%s] (%s)\n", info.Name, info.Version, info.Branch, info.Source)
				} else {
					fmt.Printf("[pbb] Package '%s' is not installed.\n", pkgName)
				}
			}
		}

	case "-q":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing search term. Example: pbb -q python")
			os.Exit(1)
		}
		searchTerm := packages[0]
		fmt.Printf("[pbb] Searching for '%s' in repository indexes...\n", searchTerm)
		if err := searchPackagesGlobal(searchTerm); err != nil {
			fmt.Printf("[pbb] Search error: %v. Try running pbb -Syu first.\n", err)
		}

	case "-AUR":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing search term. Example: pbb -AUR telegram")
			os.Exit(1)
		}
		searchTerm := packages[0]

		fmt.Printf("[pbb] Searching official repositories for '%s'...\n", searchTerm)
		_ = searchPackagesGlobal(searchTerm)

		fmt.Printf("\n[pbb] Querying AUR for '%s'...\n", searchTerm)
		if err := searchAur(searchTerm); err != nil {
			fmt.Printf("[pbb] AUR query failed: %v\n", err)
		}

	case "-S-AUR":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing package name. Example: pbb -S-AUR ponysay-git")
			os.Exit(1)
		}
		pkgName := packages[0]
		fmt.Printf("[pbb] Fetching AUR snapshot for: %s\n", pkgName)

		if err := downloadAndExtractAurSnapshot(pkgName); err != nil {
			fmt.Printf("[pbb] Failed to fetch AUR snapshot: %v\n", err)
			os.Exit(1)
		}

		buildDir := filepath.Join("/tmp", pkgName)
		fmt.Printf("[pbb] Parsing dependencies from PKGBUILD...\n")

		deps, err := parseAurDependencies(buildDir)
		if err != nil {
			fmt.Printf("[pbb] Failed to parse PKGBUILD dependencies: %v\n", err)
			os.Exit(1)
		}

		// AUR dependencies go into AurTargetPrefix to stay isolated from repo packages
		if err := CheckAndInstallDependencies(deps, currentMirror, branchName, AurTargetPrefix); err != nil {
			fmt.Printf("[pbb] Dependency installation failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("\n[+] Dependencies satisfied. Starting build pipeline...")

		pkgDir := filepath.Join("/tmp", "pbb-root-"+pkgName)
		if err := os.MkdirAll(pkgDir, 0755); err != nil {
			fmt.Printf("[pbb] Failed to create build staging directory: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("[pbb] Running build() and package() from PKGBUILD...")
		if err := runAurBuildAndPackage(buildDir, pkgDir); err != nil {
			fmt.Printf("[pbb] Build failed: %v\n", err)
			os.RemoveAll(pkgDir)
			os.Exit(1)
		}

		fmt.Println("[pbb] Build finished. Deploying files into AUR prefix...")
		// AUR packages go into AurTargetPrefix with symlinks in AurBinSymlinkDir
		manifest, err := deployBuiltFiles(pkgDir, AurTargetPrefix, AurBinSymlinkDir)
		if err != nil {
			fmt.Printf("[pbb] Deployment failed: %v\n", err)
			os.RemoveAll(pkgDir)
			os.Exit(1)
		}

		if err := saveManifest(pkgName, manifest); err != nil {
			fmt.Printf("[pbb] Failed to save manifest: %v\n", err)
		}

		registerPackage(pkgName, "git-custom", branchName, "aur")

		os.RemoveAll(pkgDir)
		os.RemoveAll(buildDir)

		fmt.Printf("\n[+] AUR package '%s' successfully built and installed!\n", pkgName)
		fmt.Printf("    Binaries available in: %s\n", AurBinSymlinkDir)

	default:
		printHelp()
	}
}
