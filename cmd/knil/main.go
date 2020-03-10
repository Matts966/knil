package main

import (
	"github.com/Matts966/knil/pkg"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() { singlechecker.Main(knil.Analyzer) }
