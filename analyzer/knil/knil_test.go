// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package knil_test

import (
	"testing"

	"github.com/Matts966/knil/analyzer/knil"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
)

func Test(t *testing.T) {
	analysis.Validate([]*analysis.Analyzer{knil.Analyzer})
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, knil.Analyzer, "nil")
}
