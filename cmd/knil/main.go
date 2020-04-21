package main

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/Matts966/knil/analyzer/knil"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/pointer"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	initial, err := load(os.Args, true)
	if err != nil {
		log.Fatal(err) // load errors
	}

	// Create and build SSA-form program representation.
	prog, pkgs := ssautil.AllPackages(initial, 0)
	prog.Build()
	mains, err := mainPackages(pkgs)
	if err != nil {
		log.Fatal(err)
	}
	config := &pointer.Config{
		Mains:          mains,
		BuildCallGraph: true,
	}
	result, err := pointer.Analyze(config)
	if err != nil {
		log.Fatal(err)
	}

	knil.Main(result.CallGraph)
}

var ignoreFilesRegexp = `.*_test.go|zz_generated.*`

func isIgnored(fn string) bool {
	m, err := regexp.MatchString(ignoreFilesRegexp, fn)
	if err != nil {
		panic(err)
	}
	return m
}

// mainPackages returns the main packages to analyze.
// Each resulting package is named "main" and has a main function.
func mainPackages(pkgs []*ssa.Package) ([]*ssa.Package, error) {
	var mains []*ssa.Package
	for _, p := range pkgs {
		if p != nil && p.Pkg.Name() == "main" && p.Func("main") != nil {
			if strings.HasSuffix(p.Pkg.Path(), ".test") {
				continue
			}
			mains = append(mains, p)
		}
	}
	if len(mains) == 0 {
		return nil, fmt.Errorf("no main packages")
	}
	return mains, nil
}

// load loads the initial packages.
func load(patterns []string, allSyntax bool) ([]*packages.Package, error) {
	mode := packages.LoadSyntax
	if allSyntax {
		mode = packages.LoadAllSyntax
	}
	conf := packages.Config{
		Mode:  mode,
		Tests: true,
	}
	initial, err := packages.Load(&conf, patterns...)
	if err != nil {
		if n := packages.PrintErrors(initial); n > 1 {
			err = fmt.Errorf("%d errors during loading", n)
		} else if n == 1 {
			err = fmt.Errorf("error during loading")
		} else if len(initial) == 0 {
			err = fmt.Errorf("%s matched no packages", strings.Join(patterns, " "))
		}
	}

	return initial, err
}
