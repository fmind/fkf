module github.com/fmind/fkf

go 1.27.0

// A module tool because govulncheck analyses this module and must be built with its Go version.
tool golang.org/x/vuln/cmd/govulncheck

require (
	github.com/google/jsonschema-go v0.4.3
	github.com/itchyny/gojq v0.12.19
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/urfave/cli/v3 v3.11.0
	github.com/yuin/goldmark v1.8.5
	go.yaml.in/yaml/v3 v3.0.5
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/itchyny/timefmt-go v0.1.8 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/mod v0.40.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260824150023-1f5465a7b7fb // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	golang.org/x/vuln v1.7.0 // indirect
)
