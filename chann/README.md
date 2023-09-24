This is a vendored version of https://github.com/amyangfei/chann/tree/should-consume-data-after-close - that exists here due to an unfixed bug in https://github.com/golang-design/chann/pull/5

`gparallel` uses a vendored version of `chann` instaad of a `replace` directive in the go.mod file, to be able to be installed with a simple `go install` - which doesn't work if there are any `replace` directives present.
