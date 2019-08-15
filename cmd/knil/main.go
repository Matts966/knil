package main

import (
	"./knil"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() { singlechecker.Main(knil.Analyzer) }