module github.com/go-keel/keel/core/kdb/postgres

go 1.27.0

require (
	github.com/go-keel/keel v0.1.0
	github.com/jackc/pgx/v5 v5.12.0
	go.opentelemetry.io/otel v1.47.0
	gorm.io/driver/postgres v1.6.3
	gorm.io/gorm v1.31.2
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.2 // indirect
	buf.build/go/protovalidate v1.4.0 // indirect
	cel.dev/cel-go v0.32.0 // indirect
	cel.dev/expr v0.25.3 // indirect
	github.com/XSAM/otelsql v0.44.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-kratos/kratos/v3 v3.0.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20260820142414-ca536658362e // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261019180522-ce908d29299b // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Points at the repo root permanently (not removed at release): replace only applies to the main module, so
// users depending on this module ignore it, while inside this repo the module always builds against the core
// at the same commit. Same approach as Kratos contrib.
replace github.com/go-keel/keel => ../../..
