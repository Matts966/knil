package knil_test

import (
	"testing"

	"github.com/Matts966/knil"
	"golang.org/x/tools/go/analysis/analysistest"
)

func Test(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, knil.Analyzer, "nil")
}