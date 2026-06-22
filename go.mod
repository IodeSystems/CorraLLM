module github.com/iodesystems/corrallm

go 1.26.2

require (
	github.com/danielgtaylor/huma/v2 v2.37.3
	github.com/go-chi/chi/v5 v5.3.0
	github.com/iodesystems/gwag v1.1.0-rc.5
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.51.0
)

require (
	connectrpc.com/connect v1.19.2 // indirect
	github.com/IodeSystems/graphql-go v1.1.0 // indirect
	github.com/bufbuild/protocompile v0.14.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/getkin/kin-openapi v0.138.0 // indirect
	github.com/go-openapi/jsonpointer v0.23.1 // indirect
	github.com/go-openapi/swag/jsonname v0.26.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/mailru/easyjson v0.9.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/oasdiff/yaml v0.0.9 // indirect
	github.com/oasdiff/yaml3 v0.0.12 // indirect
	github.com/perimeterx/marshmallow v1.1.5 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/woodsbury/decimal128 v1.4.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260504160031-60b97b32f348 // indirect
	google.golang.org/grpc v1.81.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	nhooyr.io/websocket v1.8.17 // indirect
)

// gat lives in the iodesystems monorepo libs/ tree; same pattern as redline2.
replace github.com/iodesystems/gwag => ../../libs/gwag
