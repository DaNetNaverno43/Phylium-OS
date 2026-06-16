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
	TargetPrefix     string // Root prefix destination for extraction (~/.local/share/pbb/root)
	BinSymlinkDir    string // Global environment location for binary mapping (~/.local/bin)
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

type PackageInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Branch  string `json:"branch"`
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
type typeStringList = string

func init() {
	// Locate system configuration user directory
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("[pbb] Critical error: failed to determine user home directory: %v\n", err)
		os.Exit(1)
	}

	// Establish local storage structure inside paths variables
	BasePbbDir = filepath.Join(home, ".local", "share", "pbb")
	PbbDir = filepath.Join(BasePbbDir, "system") 
	TargetPrefix = filepath.Join(BasePbbDir, "root") 
	BinSymlinkDir = filepath.Join(home, ".local", "bin") 

	StateFilePath = filepath.Join(PbbDir, "state.json")
	LocalDbPath = filepath.Join(PbbDir, "local_db.json")
	ManifestsDir = filepath.Join(PbbDir, "manifests")
	SyncDir = filepath.Join(PbbDir, "sync")
}

func main() {
	// Verify that the patchelf executable is present in host environment
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
	var packages []typeStringList

	if action == "-S" || action == "-R" || action == "-Q" || !strings.HasPrefix(action, "-") {
		fs := flag.NewFlagSet("pbb", flag.ExitOnError)
		fs.BoolVar(&bleedingOpt, "bleeding", false, "Enable tracking of bleeding edge branch components")
		fs.BoolVar(&rollbackOpt, "rollback", false, "Rollback tracked packages down from bleeding edge to stable")
		fs.BoolVar(&verboseOpt, "v", false, "Enable verbose troubleshooting log output")
		
		fs.Parse(os.Args[2:])
		packages = fs.Args()
		verbose = verboseOpt
	} else {
		packages = os.Args[2:]
	}

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

	switch action {
	case "-Syu":
		fmt.Println("[pbb] Synchronizing package repository databases...")
		repositories := []string{"core", "extra", "multilib"}

		for _, repo := range repositories {
			dbURL := fmt.Sprintf("%s/%s/os/x86_64/%s.db", currentMirror, repo, repo)
			localDbTarget := filepath.Join(SyncDir, repo+".db")

			if verbose {
				fmt.Printf("[v] Querying remote repository database database index %s: %s\n", repo, dbURL)
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
				pkgName = strings.Split(pkgName, "-")[0]
			}

			fmt.Printf("[pbb] Searching for package '%s' in repository indexes...\n", pkgName)
			repoType, pkgFilename, pkgVersion, err := searchPackageInRepositories(pkgName)
			if err != nil {
				fmt.Printf("[pbb] Error matching target component: %v. Please execute pbb -Syu first\n", err)
				continue
			}

			if verbose {
				fmt.Printf("[v] Repository hit: %s | Extracted filename: %s | Target version: %s\n", repoType, pkgFilename, pkgVersion)
			}

			url := fmt.Sprintf("%s/%s/os/x86_64/%s", currentMirror, repoType, pkgFilename)
			fmt.Printf("[pbb] Downloading target component '%s' from [%s]...\n", pkgName, repoType)

			tmpFile := filepath.Join("/tmp", pkgFilename)
			if err := downloadFile(url, tmpFile); err != nil {
				fmt.Printf("[pbb] Download process halted: %v\n", err)
				continue
			}

			if verbose {
				fmt.Printf("[v] Unpacking payload file %s into targeted system prefix environment: %s...\n", pkgFilename, TargetPrefix)
			}
			
			manifest, err := extractZstTar(tmpFile, TargetPrefix)
			if err != nil {
				fmt.Printf("[pbb] Prefix system environment layout manipulation error: %v\n", err)
				os.Remove(tmpFile)
				continue
			}

			if err := saveManifest(pkgName, manifest); err != nil {
				fmt.Printf("[pbb] Failed to finalize configuration file manifest output: %v\n", err)
			}

			registerPackage(pkgName, pkgVersion, branchName)
			os.Remove(tmpFile)
			fmt.Printf("[+] Package '%s' successfully linked to environment prefix configuration location [%s].\n", pkgName, branchName)
		}

	case "-R":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: No targets specified for deletion.")
			os.Exit(1)
		}
		for _, pkg := range packages {
			cleanPkgName := pkg
			if strings.HasSuffix(cleanPkgName, ".pkg.tar.zst") {
				cleanPkgName = strings.Split(cleanPkgName, "-")[0]
			}

			if err := removePackageWithManifest(cleanPkgName); err != nil {
				fmt.Printf("[pbb] Deletion failure on target element '%s': %v\n", cleanPkgName, err)
			}
		}

	case "-Q":
		db, err := readLocalDb()
		if err != nil {
			fmt.Printf("[pbb] Local database instance state recovery error: %v\n", err)
			os.Exit(1)
		}

		if len(packages) == 0 {
			fmt.Println("[pbb] Tracked local system repository components info:")
			for name, info := range db {
				fmt.Printf("%s %s [%s]\n", name, info.Version, info.Branch)
			}
		} else {
			for _, pkgName := range packages {
				info, exists := db[pkgName]
				if exists {
					fmt.Printf("%s %s [%s]\n", info.Name, info.Version, info.Branch)
				} else {
					fmt.Printf("[pbb] Target instance element '%s' is not tracked in the current environment.\n", pkgName)
				}
			}
		}

	case "-q":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing query expression argument. Example usage: pbb -q python")
			os.Exit(1)
		}
		searchTerm := packages[0]
		fmt.Printf("[pbb] Querying index structures for matching patterns: '%s'...\n", searchTerm)
		if err := searchPackagesGlobal(searchTerm); err != nil {
			fmt.Printf("[pbb] Remote index file query loop returned an error: %v. Database refresh may be required (pbb -Syu)\n", err)
		}

	case "-AUR":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Missing target search argument. Example usage: pbb -AUR telegram")
			os.Exit(1)
		}
		searchTerm := packages[0]

		fmt.Printf("[pbb] Scanning repository files for matching instances: '%s'...\n", searchTerm)
		_ = searchPackagesGlobal(searchTerm)

		fmt.Printf("\n[pbb] Contacting remote AUR server database backend endpoint...")
		if err := searchAur(searchTerm); err != nil {
			fmt.Printf("\n[pbb] Remote database search request processing failure: %v\n", err)
		}

	case "-S-AUR":
		if len(packages) == 0 {
			fmt.Println("[pbb] Error: Target package definition parameter missing. Example usage: pbb -S-AUR ponysay-git")
			os.Exit(1)
		}
		pkgName := packages[0]
		fmt.Printf("[pbb] Requesting source context snapshots mapping for element: %s\n", pkgName)
		
		if err := downloadAndExtractAurSnapshot(pkgName); err != nil {
			fmt.Printf("[pbb] Package directory retrieval processing failure: %v\n", err)
			os.Exit(1)
		}

		buildDir := filepath.Join("/tmp", pkgName)
		fmt.Printf("[pbb] Parsing build configuration targets in %s...\n", pkgName)
		
		deps, err := parseAurDependencies(buildDir)
		if err != nil {
			fmt.Printf("[pbb] Configuration parameters evaluation error inside package definition script: %v\n", err)
			os.Exit(1)
		}

		if err := CheckAndInstallDependencies(deps, currentMirror, branchName); err != nil {
			fmt.Printf("[pbb] Critical dependency validation check error detected: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("\n[+] Verification sequence completed. Instantiating compilation framework process pipeline...")
		
		pkgDir := filepath.Join("/tmp", "pbb-root-"+pkgName)
		if err := os.MkdirAll(pkgDir, 0755); err != nil {
			fmt.Printf("[pbb] Failed to construct runtime sandbox staging location directory structure: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("[pbb] Initializing build() and package() runtime hooks inside definition file context...")
		if err := runAurBuildAndPackage(buildDir, pkgDir); err != nil {
			fmt.Printf("[pbb] Compilation failure inside sandbox execution runtime pipeline: %v\n", err)
			os.RemoveAll(pkgDir)
			os.Exit(1)
		}

		fmt.Println("[pbb] Build pipeline finished. Relocating data objects into targeted structure mappings...")
		manifest, err := deployBuiltFiles(pkgDir, TargetPrefix)
		if err != nil {
			fmt.Printf("[pbb] Failure inside deployment sequence tracker logic module: %v\n", err)
			os.RemoveAll(pkgDir)
			os.Exit(1)
		}

		if err := saveManifest(pkgName, manifest); err != nil {
			fmt.Printf("[pbb] Manifest tracking log writing error: %v\n", err)
		}

		registerPackage(pkgName, "git-custom", branchName)
		
		os.RemoveAll(pkgDir)
		os.RemoveAll(buildDir)

		fmt.Printf("\n[+] AUR package targets component '%s' successfully built and linked inside local workspace environments!\n", pkgName)
	
	default:
		if rollbackOpt {
			if len(packages) == 0 {
				fmt.Println("[pbb] Error: Missing rollback elements targets.")
				os.Exit(1)
			}
			for _, pkg := range packages {
				handleRollback(pkg, stableMirror)
			}
		} else {
			printHelp()
		}
	}
}
