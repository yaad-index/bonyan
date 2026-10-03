module github.com/yaad-index/bonyan/memory/honcho

go 1.26.0

require (
	github.com/stretchr/testify v1.12.1
	github.com/yaad-index/bonyan v0.0.0-00010101000000-000000000000
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

// Development builds against the root module in this repository. A replace
// directive applies only when this module is the main module, so callers
// never see it: a release of this module first raises the requirement above
// to a released root version (ADR 0002).
replace github.com/yaad-index/bonyan => ../..
