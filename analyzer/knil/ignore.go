package knil

// This file contains processes for ignoring files related to
// dynamic analysis. We can ignore them because they are also
// for making code safer and they do not run on production
// environment.

import (
	"regexp"

	"golang.org/x/tools/go/ssa"
)

var ignoreFilesRegexp = `.*_test.go|zz_generated.*`

func isIgnoredFunction(f *ssa.Function) bool {
	m, err := regexp.MatchString(ignoreFilesRegexp, getFileNameOfFunction(f))
	if err != nil {
		panic(err)
	}
	return m
}

func getFileNameOfFunction(f *ssa.Function) string {
	fs := f.Prog.Fset
	return fs.File(f.Pos()).Name()
}
