package main

import (
	"bufio"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/github/codeql-go/extractor/autobuilder"
	"github.com/github/codeql-go/extractor/diagnostics"
	"github.com/github/codeql-go/extractor/util"
)

func usage() {
	fmt.Fprintf(os.Stderr,
		`%s is a wrapper script that installs dependencies and calls the extractor

Options:
  --identify-environment
    Produce an environment file specifying which Go version should be installed in the environment
	so that autobuilding will be successful. The location of this file is controlled by the
    environment variable CODEQL_EXTRACTOR_ENVIRONMENT_JSON, or defaults to 'environment.json' if
	that is not set.

Build behavior:

    When LGTM_SRC is not set, the script installs dependencies as described below, and then invokes the
    extractor in the working directory.

    If LGTM_SRC is set, it checks for the presence of the files 'go.mod', 'Gopkg.toml', and
    'glide.yaml' to determine how to install dependencies: if a 'Gopkg.toml' file is present, it uses
    'dep ensure', if there is a 'glide.yaml' it uses 'glide install', and otherwise 'go get'.
    Additionally, unless a 'go.mod' file is detected, it sets up a temporary GOPATH and moves all
    source files into a folder corresponding to the package's import path before installing
    dependencies.

    This behavior can be further customized using environment variables: setting LGTM_INDEX_NEED_GOPATH
    to 'false' disables the GOPATH set-up, CODEQL_EXTRACTOR_GO_BUILD_COMMAND (or alternatively
    LGTM_INDEX_BUILD_COMMAND), can be set to a newline-separated list of commands to run in order to
    install dependencies, and LGTM_INDEX_IMPORT_PATH can be used to override the package import path,
    which is otherwise inferred from the SEMMLE_REPO_URL or GITHUB_REPOSITORY environment variables.    

    In resource-constrained environments, the environment variable CODEQL_EXTRACTOR_GO_MAX_GOROUTINES
    (or its legacy alias SEMMLE_MAX_GOROUTINES) can be used to limit the number of parallel goroutines
    started by the extractor, which reduces CPU and memory requirements. The default value for this
    variable is 32.
`,
		os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage:\n\n  %s\n", os.Args[0])
}

var goVersion = ""

// Returns the current Go version as returned by 'go version', e.g. go1.14.4
func getEnvGoVersion() string {
	if goVersion == "" {
		gover, err := exec.Command("go", "version").CombinedOutput()
		if err != nil {
			log.Fatalf("Unable to run the go command, is it installed?\nError: %s", err.Error())
		}
		goVersion = parseGoVersion(string(gover))
	}
	return goVersion
}

// The 'go version' command may output warnings on separate lines before
// the actual version string is printed. This function parses the output
// to retrieve just the version string.
func parseGoVersion(data string) string {
	var lastLine string
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		lastLine = sc.Text()
	}
	return strings.Fields(lastLine)[2]
}

// Returns the current Go version in semver format, e.g. v1.14.4
func getEnvGoSemVer() string {
	goVersion := getEnvGoVersion()
	if !strings.HasPrefix(goVersion, "go") {
		log.Fatalf("Expected 'go version' output of the form 'go1.2.3'; got '%s'", goVersion)
	}
	return "v" + goVersion[2:]
}

// Returns the import path of the package being built, or "" if it cannot be determined.
func getImportPath() (importpath string) {
	importpath = os.Getenv("LGTM_INDEX_IMPORT_PATH")
	if importpath == "" {
		repourl := os.Getenv("SEMMLE_REPO_URL")
		if repourl == "" {
			githubrepo := os.Getenv("GITHUB_REPOSITORY")
			if githubrepo == "" {
				log.Printf("Unable to determine import path, as neither LGTM_INDEX_IMPORT_PATH nor GITHUB_REPOSITORY is set\n")
				return ""
			} else {
				importpath = "github.com/" + githubrepo
			}
		} else {
			importpath = getImportPathFromRepoURL(repourl)
			if importpath == "" {
				log.Printf("Failed to determine import path from SEMMLE_REPO_URL '%s'\n", repourl)
				return
			}
		}
	}
	log.Printf("Import path is '%s'\n", importpath)
	return
}

// Returns the import path of the package being built from `repourl`, or "" if it cannot be
// determined.
func getImportPathFromRepoURL(repourl string) string {
	// check for scp-like URL as in "git@github.com:github/codeql-go.git"
	shorturl := regexp.MustCompile(`^([^@]+@)?([^:]+):([^/].*?)(\.git)?$`)
	m := shorturl.FindStringSubmatch(repourl)
	if m != nil {
		return m[2] + "/" + m[3]
	}

	// otherwise parse as proper URL
	u, err := url.Parse(repourl)
	if err != nil {
		log.Fatalf("Malformed repository URL '%s'\n", repourl)
	}

	if u.Scheme == "file" {
		// we can't determine import paths from file paths
		return ""
	}

	if u.Hostname() == "" || u.Path == "" {
		return ""
	}

	host := u.Hostname()
	path := u.Path
	// strip off leading slashes and trailing `.git` if present
	path = regexp.MustCompile(`^/+|\.git$`).ReplaceAllString(path, "")
	return host + "/" + path
}

func restoreRepoLayout(fromDir string, dirEntries []string, scratchDirName string, toDir string) {
	for _, dirEntry := range dirEntries {
		if dirEntry != scratchDirName {
			log.Printf("Restoring %s/%s to %s/%s.\n", fromDir, dirEntry, toDir, dirEntry)
			err := os.Rename(filepath.Join(fromDir, dirEntry), filepath.Join(toDir, dirEntry))
			if err != nil {
				log.Printf("Failed to move file/directory %s from directory %s to directory %s: %s\n", dirEntry, fromDir, toDir, err.Error())
			}
		}
	}
}

// DependencyInstallerMode is an enum describing how dependencies should be installed
type DependencyInstallerMode int

const (
	// GoGetNoModules represents dependency installation using `go get` without modules
	GoGetNoModules DependencyInstallerMode = iota
	// GoGetWithModules represents dependency installation using `go get` with modules
	GoGetWithModules
	// Dep represent dependency installation using `dep ensure`
	Dep
	// Glide represents dependency installation using `glide install`
	Glide
)

// ModMode corresponds to the possible values of the -mod flag for the Go compiler
type ModMode int

const (
	ModUnset ModMode = iota
	ModReadonly
	ModMod
	ModVendor
)

// argsForGoVersion returns the arguments to pass to the Go compiler for the given `ModMode` and
// Go version
func (m ModMode) argsForGoVersion(version string) []string {
	switch m {
	case ModUnset:
		return []string{}
	case ModReadonly:
		return []string{"-mod=readonly"}
	case ModMod:
		if !semver.IsValid(version) {
			log.Fatalf("Invalid Go semver: '%s'", version)
		}
		if semver.Compare(version, "v1.14") < 0 {
			return []string{} // -mod=mod is the default behaviour for go <= 1.13, and is not accepted as an argument
		} else {
			return []string{"-mod=mod"}
		}
	case ModVendor:
		return []string{"-mod=vendor"}
	}
	return nil
}

// addVersionToMod add a go version directive, e.g. `go 1.14` to a `go.mod` file.
func addVersionToMod(version string) bool {
	cmd := exec.Command("go", "mod", "edit", "-go="+version)
	return util.RunCmd(cmd)
}

// checkVendor tests to see whether a vendor directory is inconsistent according to the go frontend
func checkVendor() bool {
	vendorCheckCmd := exec.Command("go", "list", "-mod=vendor", "./...")
	outp, err := vendorCheckCmd.CombinedOutput()
	if err != nil {
		badVendorRe := regexp.MustCompile(`(?m)^go: inconsistent vendoring in .*:$`)
		return !badVendorRe.Match(outp)
	}

	return true
}

// Returns the directory containing the source code to be analyzed.
func getSourceDir() string {
	srcdir := os.Getenv("LGTM_SRC")
	if srcdir != "" {
		log.Printf("LGTM_SRC is %s\n", srcdir)
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatalln("Failed to get current working directory.")
		}
		log.Printf("LGTM_SRC is not set; defaulting to current working directory %s\n", cwd)
		srcdir = cwd
	}
	return srcdir
}

// Returns the appropriate DependencyInstallerMode for the current project
func getDepMode() DependencyInstallerMode {
	if util.FileExists("go.mod") {
		log.Println("Found go.mod, enabling go modules")
		return GoGetWithModules
	}
	if util.FileExists("Gopkg.toml") {
		log.Println("Found Gopkg.toml, using dep instead of go get")
		return Dep
	}
	if util.FileExists("glide.yaml") {
		log.Println("Found glide.yaml, enabling go modules")
		return Glide
	}
	return GoGetNoModules
}

// Tries to open `go.mod` and read a go directive, returning the version and whether it was found.
func tryReadGoDirective(depMode DependencyInstallerMode) (string, bool) {
	if depMode == GoGetWithModules {
		versionRe := regexp.MustCompile(`(?m)^go[ \t\r]+([0-9]+\.[0-9]+)$`)
		goMod, err := os.ReadFile("go.mod")
		if err != nil {
			log.Println("Failed to read go.mod to check for missing Go version")
		} else {
			matches := versionRe.FindSubmatch(goMod)
			if matches != nil {
				if len(matches) > 1 {
					return string(matches[1]), true
				}
			}
		}
	}
	return "", false
}

// Returns the appropriate ModMode for the current project
func getModMode(depMode DependencyInstallerMode) ModMode {
	if depMode == GoGetWithModules {
		// if a vendor/modules.txt file exists, we assume that there are vendored Go dependencies, and
		// skip the dependency installation step and run the extractor with `-mod=vendor`
		if util.FileExists("vendor/modules.txt") {
			return ModVendor
		} else if util.DirExists("vendor") {
			return ModMod
		}
	}
	return ModUnset
}

// fixGoVendorIssues fixes issues with go vendor for go version >= 1.14
func fixGoVendorIssues(modMode ModMode, depMode DependencyInstallerMode, goModVersionFound bool) ModMode {
	if modMode == ModVendor {
		// fix go vendor issues with go versions >= 1.14 when no go version is specified in the go.mod
		// if this is the case, and dependencies were vendored with an old go version (and therefore
		// do not contain a '## explicit' annotation, the go command will fail and refuse to do any
		// work
		//
		// we work around this by adding an explicit go version of 1.13, which is the last version
		// where this is not an issue
		if depMode == GoGetWithModules {
			if !goModVersionFound {
				// if the go.mod does not contain a version line
				modulesTxt, err := os.ReadFile("vendor/modules.txt")
				if err != nil {
					log.Println("Failed to read vendor/modules.txt to check for mismatched Go version")
				} else if explicitRe := regexp.MustCompile("(?m)^## explicit$"); !explicitRe.Match(modulesTxt) {
					// and the modules.txt does not contain an explicit annotation
					log.Println("Adding a version directive to the go.mod file as the modules.txt does not have explicit annotations")
					if !addVersionToMod("1.13") {
						log.Println("Failed to add a version to the go.mod file to fix explicitly required package bug; not using vendored dependencies")
						return ModMod
					}
				}
			}
		}
	}
	return modMode
}

// Determines whether the project needs a GOPATH set up
func getNeedGopath(depMode DependencyInstallerMode, importpath string) bool {
	needGopath := true
	if depMode == GoGetWithModules {
		needGopath = false
	}
	// if `LGTM_INDEX_NEED_GOPATH` is set, it overrides the value for `needGopath` inferred above
	if needGopathOverride := os.Getenv("LGTM_INDEX_NEED_GOPATH"); needGopathOverride != "" {
		if needGopathOverride == "true" {
			needGopath = true
		} else if needGopathOverride == "false" {
			needGopath = false
		} else {
			log.Fatalf("Unexpected value for Boolean environment variable LGTM_NEED_GOPATH: %v.\n", needGopathOverride)
		}
	}
	if needGopath && importpath == "" {
		log.Printf("Failed to determine import path, not setting up GOPATH")
		needGopath = false
	}
	return needGopath
}

// Try to update `go.mod` and `go.sum` if the go version is >= 1.16.
func tryUpdateGoModAndGoSum(modMode ModMode, depMode DependencyInstallerMode) {
	// Go 1.16 and later won't automatically attempt to update go.mod / go.sum during package loading, so try to update them here:
	if modMode != ModVendor && depMode == GoGetWithModules && semver.Compare(getEnvGoSemVer(), "v1.16") >= 0 {
		// stat go.mod and go.sum
		beforeGoModFileInfo, beforeGoModErr := os.Stat("go.mod")
		if beforeGoModErr != nil {
			log.Println("Failed to stat go.mod before running `go mod tidy -e`")
		}

		beforeGoSumFileInfo, beforeGoSumErr := os.Stat("go.sum")

		// run `go mod tidy -e`
		res := util.RunCmd(exec.Command("go", "mod", "tidy", "-e"))

		if !res {
			log.Println("Failed to run `go mod tidy -e`")
		} else {
			if beforeGoModFileInfo != nil {
				afterGoModFileInfo, afterGoModErr := os.Stat("go.mod")
				if afterGoModErr != nil {
					log.Println("Failed to stat go.mod after running `go mod tidy -e`")
				} else if afterGoModFileInfo.ModTime().After(beforeGoModFileInfo.ModTime()) {
					// if go.mod has been changed then notify the user
					log.Println("We have run `go mod tidy -e` and it altered go.mod. You may wish to check these changes into version control. ")
				}
			}

			afterGoSumFileInfo, afterGoSumErr := os.Stat("go.sum")
			if afterGoSumErr != nil {
				log.Println("Failed to stat go.sum after running `go mod tidy -e`")
			} else {
				if beforeGoSumErr != nil || afterGoSumFileInfo.ModTime().After(beforeGoSumFileInfo.ModTime()) {
					// if go.sum has been changed then notify the user
					log.Println("We have run `go mod tidy -e` and it altered go.sum. You may wish to check these changes into version control. ")
				}
			}
		}
	}
}

type moveGopathInfo struct {
	scratch, realSrc, root, newdir string
	files                          []string
}

// Moves all files in `srcdir` to a temporary directory with the correct layout to be added to the GOPATH
func moveToTemporaryGopath(srcdir string, importpath string) moveGopathInfo {
	// a temporary directory where everything is moved while the correct
	// directory structure is created.
	scratch, err := os.MkdirTemp(srcdir, "scratch")
	if err != nil {
		log.Fatalf("Failed to create temporary directory %s in directory %s: %s\n",
			scratch, srcdir, err.Error())
	}
	log.Printf("Temporary directory is %s.\n", scratch)

	// move all files in `srcdir` to `scratch`
	dir, err := os.Open(srcdir)
	if err != nil {
		log.Fatalf("Failed to open source directory %s for reading: %s\n", srcdir, err.Error())
	}
	files, err := dir.Readdirnames(-1)
	if err != nil {
		log.Fatalf("Failed to read source directory %s: %s\n", srcdir, err.Error())
	}
	for _, file := range files {
		if file != filepath.Base(scratch) {
			log.Printf("Moving %s/%s to %s/%s.\n", srcdir, file, scratch, file)
			err := os.Rename(filepath.Join(srcdir, file), filepath.Join(scratch, file))
			if err != nil {
				log.Fatalf("Failed to move file %s to the temporary directory: %s\n", file, err.Error())
			}
		}
	}

	// create a new folder which we will add to GOPATH below
	// Note we evaluate all symlinks here for consistency: otherwise os.Chdir below
	// will follow links but other references to the path may not, which can lead to
	// disagreements between GOPATH and the working directory.
	realSrc, err := filepath.EvalSymlinks(srcdir)
	if err != nil {
		log.Fatalf("Failed to evaluate symlinks in %s: %s\n", srcdir, err.Error())
	}

	root := filepath.Join(realSrc, "root")

	// move source files to where Go expects them to be
	newdir := filepath.Join(root, "src", importpath)
	err = os.MkdirAll(filepath.Dir(newdir), 0755)
	if err != nil {
		log.Fatalf("Failed to create directory %s: %s\n", newdir, err.Error())
	}
	log.Printf("Moving %s to %s.\n", scratch, newdir)
	err = os.Rename(scratch, newdir)
	if err != nil {
		log.Fatalf("Failed to rename %s to %s: %s\n", scratch, newdir, err.Error())
	}

	return moveGopathInfo{
		scratch: scratch,
		realSrc: realSrc,
		root:    root,
		newdir:  newdir,
		files:   files,
	}
}

// Creates a path transformer file in the new directory to ensure paths in the source archive and the snapshot
// match the original source location, not the location we moved it to.
func createPathTransformerFile(newdir string) *os.File {
	err := os.Chdir(newdir)
	if err != nil {
		log.Fatalf("Failed to chdir into %s: %s\n", newdir, err.Error())
	}

	// set up SEMMLE_PATH_TRANSFORMER to ensure paths in the source archive and the snapshot
	// match the original source location, not the location we moved it to
	pt, err := os.CreateTemp("", "path-transformer")
	if err != nil {
		log.Fatalf("Unable to create path transformer file: %s.", err.Error())
	}
	return pt
}

// Writes the path transformer file
func writePathTransformerFile(pt *os.File, realSrc, root, newdir string) {
	_, err := pt.WriteString("#" + realSrc + "\n" + newdir + "//\n")
	if err != nil {
		log.Fatalf("Unable to write path transformer file: %s.", err.Error())
	}
	err = pt.Close()
	if err != nil {
		log.Fatalf("Unable to close path transformer file: %s.", err.Error())
	}
	err = os.Setenv("SEMMLE_PATH_TRANSFORMER", pt.Name())
	if err != nil {
		log.Fatalf("Unable to set SEMMLE_PATH_TRANSFORMER environment variable: %s.\n", err.Error())
	}
}

// Adds `root` to GOPATH.
func setGopath(root string) {
	// set/extend GOPATH
	oldGopath := os.Getenv("GOPATH")
	var newGopath string
	if oldGopath != "" {
		newGopath = strings.Join(
			[]string{root, oldGopath},
			string(os.PathListSeparator),
		)
	} else {
		newGopath = root
	}
	err := os.Setenv("GOPATH", newGopath)
	if err != nil {
		log.Fatalf("Unable to set GOPATH to %s: %s\n", newGopath, err.Error())
	}
	log.Printf("GOPATH set to %s.\n", newGopath)
}

// Try to build the project without custom commands. If that fails, return a boolean indicating
// that we should install dependencies ourselves.
func buildWithoutCustomCommands(modMode ModMode) bool {
	shouldInstallDependencies := false
	// try to build the project
	buildSucceeded := autobuilder.Autobuild()

	// Build failed or there are still dependency errors; we'll try to install dependencies
	// ourselves
	if !buildSucceeded {
		log.Println("Build failed, continuing to install dependencies.")

		shouldInstallDependencies = true
	} else if util.DepErrors("./...", modMode.argsForGoVersion(getEnvGoSemVer())...) {
		log.Println("Dependencies are still not resolving after the build, continuing to install dependencies.")

		shouldInstallDependencies = true
	}
	return shouldInstallDependencies
}

// Build the project with custom commands.
func buildWithCustomCommands(inst string) {
	// write custom build commands into a script, then run it
	var (
		ext    = ""
		header = ""
		footer = ""
	)
	if runtime.GOOS == "windows" {
		ext = ".cmd"
		header = "@echo on\n@prompt +$S\n"
		footer = "\nIF %ERRORLEVEL% NEQ 0 EXIT"
	} else {
		ext = ".sh"
		header = "#! /bin/bash\nset -xe +u\n"
	}
	script, err := os.CreateTemp("", "go-build-command-*"+ext)
	if err != nil {
		log.Fatalf("Unable to create temporary script holding custom build commands: %s\n", err.Error())
	}
	defer os.Remove(script.Name())
	_, err = script.WriteString(header + inst + footer)
	if err != nil {
		log.Fatalf("Unable to write to temporary script holding custom build commands: %s\n", err.Error())
	}
	err = script.Close()
	if err != nil {
		log.Fatalf("Unable to close temporary script holding custom build commands: %s\n", err.Error())
	}
	os.Chmod(script.Name(), 0700)
	log.Println("Installing dependencies using custom build command.")
	util.RunCmd(exec.Command(script.Name()))
}

// Install dependencies using the given dependency installer mode.
func installDependencies(depMode DependencyInstallerMode) {
	// automatically determine command to install dependencies
	var install *exec.Cmd
	if depMode == Dep {
		// set up the dep cache if SEMMLE_CACHE is set
		cacheDir := os.Getenv("SEMMLE_CACHE")
		if cacheDir != "" {
			depCacheDir := filepath.Join(cacheDir, "go", "dep")
			log.Printf("Attempting to create dep cache dir %s\n", depCacheDir)
			err := os.MkdirAll(depCacheDir, 0755)
			if err != nil {
				log.Printf("Failed to create dep cache directory: %s\n", err.Error())
			} else {
				log.Printf("Setting dep cache directory to %s\n", depCacheDir)
				err = os.Setenv("DEPCACHEDIR", depCacheDir)
				if err != nil {
					log.Println("Failed to set dep cache directory")
				} else {
					err = os.Setenv("DEPCACHEAGE", "720h") // 30 days
					if err != nil {
						log.Println("Failed to set dep cache age")
					}
				}
			}
		}

		if util.FileExists("Gopkg.lock") {
			// if Gopkg.lock exists, don't update it and only vendor dependencies
			install = exec.Command("dep", "ensure", "-v", "-vendor-only")
		} else {
			install = exec.Command("dep", "ensure", "-v")
		}
		log.Println("Installing dependencies using `dep ensure`.")
	} else if depMode == Glide {
		install = exec.Command("glide", "install")
		log.Println("Installing dependencies using `glide install`")
	} else {
		// explicitly set go module support
		if depMode == GoGetWithModules {
			os.Setenv("GO111MODULE", "on")
		} else if depMode == GoGetNoModules {
			os.Setenv("GO111MODULE", "off")
		}

		// get dependencies
		install = exec.Command("go", "get", "-v", "./...")
		log.Println("Installing dependencies using `go get -v ./...`.")
	}
	util.RunCmd(install)
}

// Run the extractor.
func extract(depMode DependencyInstallerMode, modMode ModMode) {
	extractor, err := util.GetExtractorPath()
	if err != nil {
		log.Fatalf("Could not determine path of extractor: %v.\n", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Unable to determine current directory: %s\n", err.Error())
	}

	extractorArgs := []string{}
	if depMode == GoGetWithModules {
		extractorArgs = append(extractorArgs, modMode.argsForGoVersion(getEnvGoSemVer())...)
	}
	extractorArgs = append(extractorArgs, "./...")

	log.Printf("Running extractor command '%s %v' from directory '%s'.\n", extractor, extractorArgs, cwd)
	cmd := exec.Command(extractor, extractorArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Fatalf("Extraction failed: %s\n", err.Error())
	}
}

// Build the project and run the extractor.
func installDependenciesAndBuild() {
	log.Printf("Autobuilder was built with %s, environment has %s\n", runtime.Version(), getEnvGoVersion())

	srcdir := getSourceDir()

	// we set `SEMMLE_PATH_TRANSFORMER` ourselves in some cases, so blank it out first for consistency
	os.Setenv("SEMMLE_PATH_TRANSFORMER", "")

	// determine how to install dependencies and whether a GOPATH needs to be set up before
	// extraction
	depMode := getDepMode()
	if _, present := os.LookupEnv("GO111MODULE"); !present {
		os.Setenv("GO111MODULE", "auto")
	}

	goModVersion, goModVersionFound := tryReadGoDirective(depMode)

	if semver.Compare("v"+goModVersion, getEnvGoSemVer()) >= 0 {
		diagnostics.EmitNewerGoVersionNeeded()
	}

	modMode := getModMode(depMode)
	modMode = fixGoVendorIssues(modMode, depMode, goModVersionFound)

	tryUpdateGoModAndGoSum(modMode, depMode)

	importpath := getImportPath()
	needGopath := getNeedGopath(depMode, importpath)

	inLGTM := os.Getenv("LGTM_SRC") != "" || os.Getenv("LGTM_INDEX_NEED_GOPATH") != ""

	if inLGTM && needGopath {
		paths := moveToTemporaryGopath(srcdir, importpath)

		// schedule restoring the contents of newdir to their original location after this function completes:
		defer restoreRepoLayout(paths.newdir, paths.files, filepath.Base(paths.scratch), srcdir)

		pt := createPathTransformerFile(paths.newdir)
		defer os.Remove(pt.Name())

		writePathTransformerFile(pt, paths.realSrc, paths.root, paths.newdir)
		setGopath(paths.root)
	}

	// check whether an explicit dependency installation command was provided
	inst := util.Getenv("CODEQL_EXTRACTOR_GO_BUILD_COMMAND", "LGTM_INDEX_BUILD_COMMAND")
	shouldInstallDependencies := false
	if inst == "" {
		shouldInstallDependencies = buildWithoutCustomCommands(modMode)
	} else {
		buildWithCustomCommands(inst)
	}

	if modMode == ModVendor {
		// test if running `go` with -mod=vendor works, and if it doesn't, try to fallback to -mod=mod
		// or not set if the go version < 1.14. Note we check this post-build in case the build brings
		// the vendor directory up to date.
		if !checkVendor() {
			modMode = ModMod
			log.Println("The vendor directory is not consistent with the go.mod; not using vendored dependencies.")
		}
	}

	if shouldInstallDependencies {
		if modMode == ModVendor {
			log.Printf("Skipping dependency installation because a Go vendor directory was found.")
		} else {
			installDependencies(depMode)
		}
	}

	extract(depMode, modMode)
}

const minGoVersion = "1.11"
const maxGoVersion = "1.20"

// Check if `version` is lower than `minGoVersion` or higher than `maxGoVersion`. Note that for
// this comparison we ignore the patch part of the version, so 1.20.1 and 1.20 are considered
// equal.
func outsideSupportedRange(version string) bool {
	short := semver.MajorMinor("v" + version)
	return semver.Compare(short, "v"+minGoVersion) < 0 || semver.Compare(short, "v"+maxGoVersion) > 0
}

// Check if `v.goModVersion` or `v.goEnvVersion` are outside of the supported range. If so, emit
// a diagnostic and return an empty version to indicate that we should not attempt to install a
// different version of Go.
func checkForUnsupportedVersions(v versionInfo) (msg, version string) {
	if v.goModVersionFound && outsideSupportedRange(v.goModVersion) {
		msg = "The version of Go found in the `go.mod` file (" + v.goModVersion +
			") is outside of the supported range (" + minGoVersion + "-" + maxGoVersion +
			"). Writing an environment file not specifying any version of Go."
		version = ""
		diagnostics.EmitUnsupportedVersionGoMod(msg)
	} else if v.goEnvVersionFound && outsideSupportedRange(v.goEnvVersion) {
		msg = "The version of Go installed in the environment (" + v.goEnvVersion +
			") is outside of the supported range (" + minGoVersion + "-" + maxGoVersion +
			"). Writing an environment file not specifying any version of Go."
		version = ""
		diagnostics.EmitUnsupportedVersionEnvironment(msg)
	}

	return msg, version
}

// Check if either `v.goEnvVersionFound` or `v.goModVersionFound` are false. If so, emit
// a diagnostic and return the version to install, or the empty string if we should not attempt to
// install a version of Go. We assume that `checkForUnsupportedVersions` has already been
// called, so any versions that are found are within the supported range.
func checkForVersionsNotFound(v versionInfo) (msg, version string) {
	if !v.goEnvVersionFound && !v.goModVersionFound {
		msg = "No version of Go installed and no `go.mod` file found. Writing an environment " +
			"file specifying the maximum supported version of Go (" + maxGoVersion + ")."
		version = maxGoVersion
		diagnostics.EmitNoGoModAndNoGoEnv(msg)
	}

	if !v.goEnvVersionFound && v.goModVersionFound {
		msg = "No version of Go installed. Writing an environment file specifying the version " +
			"of Go found in the `go.mod` file (" + v.goModVersion + ")."
		version = v.goModVersion
		diagnostics.EmitNoGoEnv(msg)
	}

	if v.goEnvVersionFound && !v.goModVersionFound {
		msg = "No `go.mod` file found. Version " + v.goEnvVersion + " installed in the " +
			"environment. Writing an environment file not specifying any version of Go."
		version = ""
		diagnostics.EmitNoGoMod(msg)
	}

	return msg, version
}

// Compare `v.goModVersion` and `v.goEnvVersion`. emit a diagnostic and return the version to
// install, or the empty string if we should not attempt to install a version of Go. We assume that
// `checkForUnsupportedVersions` and `checkForVersionsNotFound` have already been called, so both
// versions are found and are within the supported range.
func compareVersions(v versionInfo) (msg, version string) {
	if semver.Compare("v"+v.goModVersion, "v"+v.goEnvVersion) > 0 {
		msg = "The version of Go installed in the environment (" + v.goEnvVersion +
			") is lower than the version found in the `go.mod` file (" + v.goModVersion +
			"). Writing an environment file specifying the version of Go from the `go.mod` " +
			"file (" + v.goModVersion + ")."
		version = v.goModVersion
		diagnostics.EmitVersionGoModHigherVersionEnvironment(msg)
	} else {
		msg = "The version of Go installed in the environment (" + v.goEnvVersion +
			") is high enough for the version found in the `go.mod` file (" + v.goModVersion +
			"). Writing an environment file not specifying any version of Go."
		version = ""
		diagnostics.EmitVersionGoModNotHigherVersionEnvironment(msg)
	}

	return msg, version
}

// Check the versions of Go found in the environment and in the `go.mod` file, and return a
// version to install. If the version is the empty string then no installation is required.
func getVersionToInstall(v versionInfo) (msg, version string) {
	msg, version = checkForUnsupportedVersions(v)
	if msg != "" {
		return msg, version
	}

	msg, version = checkForVersionsNotFound(v)
	if msg != "" {
		return msg, version
	}

	msg, version = compareVersions(v)
	return msg, version
}

// Write an environment file to the current directory. If `version` is the empty string then
// write an empty environment file, otherwise write an environment file specifying the version
// of Go to install. The path to the environment file is specified by the
// CODEQL_EXTRACTOR_ENVIRONMENT_JSON environment variable, or defaults to `environment.json`.
func writeEnvironmentFile(version string) {
	var content string
	if version == "" {
		content = `{ "include": [] }`
	} else {
		content = `{ "include": [ { "go": { "version": "` + version + `" } } ] }`
	}

	filename, ok := os.LookupEnv("CODEQL_EXTRACTOR_ENVIRONMENT_JSON")
	if !ok {
		filename = "environment.json"
	}

	targetFile, err := os.Create(filename)
	if err != nil {
		log.Println("Failed to create environment file " + filename + ": ")
		log.Println(err)
		return
	}
	defer func() {
		if err := targetFile.Close(); err != nil {
			log.Println("Failed to close environment file " + filename + ":")
			log.Println(err)
		}
	}()

	_, err = targetFile.WriteString(content)
	if err != nil {
		log.Println("Failed to write to environment file " + filename + ": ")
		log.Println(err)
	}
}

type versionInfo struct {
	goModVersion      string // The version of Go found in the go directive in the `go.mod` file.
	goModVersionFound bool   // Whether a `go` directive was found in the `go.mod` file.
	goEnvVersion      string // The version of Go found in the environment.
	goEnvVersionFound bool   // Whether an installation of Go was found in the environment.
}

func (v versionInfo) String() string {
	return fmt.Sprintf(
		"go.mod version: %s, go.mod directive found: %t, go env version: %s, go installation found: %t",
		v.goModVersion, v.goModVersionFound, v.goEnvVersion, v.goEnvVersionFound)
}

// Check if Go is installed in the environment.
func isGoInstalled() bool {
	_, err := exec.LookPath("go")
	return err == nil
}

// Get the version of Go to install and write it to an environment file.
func identifyEnvironment() {
	var v versionInfo
	depMode := getDepMode()
	v.goModVersion, v.goModVersionFound = tryReadGoDirective(depMode)

	v.goEnvVersionFound = isGoInstalled()
	if v.goEnvVersionFound {
		v.goEnvVersion = getEnvGoVersion()[2:]
	}

	msg, versionToInstall := getVersionToInstall(v)
	log.Println(msg)

	writeEnvironmentFile(versionToInstall)
}

func main() {
	if len(os.Args) == 1 {
		installDependenciesAndBuild()
	} else if len(os.Args) == 2 && os.Args[1] == "--identify-environment" {
		identifyEnvironment()
	} else {
		usage()
		os.Exit(2)
	}
}
