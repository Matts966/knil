package main

import (
	"github.com/Matts966/knil"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() { singlechecker.Main(knil.Analyzer) }