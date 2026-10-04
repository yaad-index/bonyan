module github.com/yaad-index/bonyan/memory/sqlite

go 1.26.0

require (
	github.com/stretchr/testify v1.12.1
	github.com/yaad-index/bonyan v0.1.0
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// Development builds against the root module in this repository. A replace
// directive applies only when this module is the main module, so callers
// never see it: a release of this module first raises the requirement above
// to a released root version (ADR 0002).
replace github.com/yaad-index/bonyan => ../..
