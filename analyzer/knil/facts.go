package knil

import (
	"fmt"
	"go/token"
)

type functionInfo struct {
	// na has the information about arguments which can be nil.
	na nilnesses
	nr nilnesses

	// Receiver free variables
	rfvs nilnesses
}

func (fi functionInfo) String() string {
	return fmt.Sprintf("arguments: %v, return value: %v, potential free variable: %v", fi.na, fi.nr, fi.rfvs)
}

type receiverFreeVariables nilnesses

func (*functionInfo) AFact() {}

type pkgDone struct{}

func (pkgDone) String() string { return "done"}

func (*pkgDone) AFact() {}

type alreadyReportedGlobal struct{}

func (alreadyReportedGlobal) String() string { return "already reported global"}

func (*alreadyReportedGlobal) AFact() {}

// posToArgnilness holds nilness information
// of functions calls by tokne.Pos.
var posToArgnilness = make(map[token.Pos]nilnesses)
