# knil
[![build & test](https://github.com/Matts966/knil/workflows/build%20&%20test/badge.svg)](https://github.com/Matts966/knil/actions)


Sound checker of nil pointer dereference almost based on "golang.org/x/tools/go/analysis/passes/nilness".

```bash
# to install
go get github.com/Matts966/knil/cmd/singleknil # for a package level analysis
go get github.com/Matts966/knil/cmd/knil # including deps

# to run on a package
(cd package-dir && singleknil ./...)
# to run on a package including dependencies
(cd package-dir && knil ./...)
```
