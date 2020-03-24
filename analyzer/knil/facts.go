package knil

import (
	"fmt"
)

type functionInfo struct {
	// na has the information about arguments which can be nil.
	na posToNilnesses
	nr posToNilnesses

	// Receiver free variables
	rfv posToNilness
}

func (fi functionInfo) String() string {
	return fmt.Sprintf("arguments: %v, return value: %v, potential free variable: %v", fi.na, fi.nr, fi.rfv)
}

func (*functionInfo) AFact() {}

type pkgDone struct{}

func (pkgDone) String() string { return "done" }

func (*pkgDone) AFact() {}

type alreadyReportedGlobal struct{}

func (alreadyReportedGlobal) String() string { return "already reported global" }

func (*alreadyReportedGlobal) AFact() {}
