package knil

type functionInfo struct {
	// na has the information about arguments which can be nil.
	na   []nilness
	nr   []nilness
	rfvs []nilness
}

type receiverFreeVariables []nilness

func (*functionInfo) AFact() {}

type pkgDone struct{}

func (*pkgDone) AFact() {}

type alreadyReportedGlobal struct{}

func (*alreadyReportedGlobal) AFact() {}
