package knil

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
