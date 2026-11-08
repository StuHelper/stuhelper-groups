module github.com/StuHelper/stuhelper-groups

go 1.27.3

require (
	github.com/StuHelper/stuhelper-platform v0.0.0-20261108152919-0efdba00ff59
	github.com/StuHelper/stuhelper-qq-binding v0.0.0-20261108161113-47f1d06881af
	github.com/StuHelper/stuhelper-verification v0.0.0-20261108161229-d9045908aa41
	github.com/getkin/kin-openapi v0.149.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/oapi-codegen/runtime v1.8.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/go-openapi/jsonpointer v0.22.5 // indirect
	github.com/go-openapi/swag/jsonname v0.25.5 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.32.0 // indirect
)

replace github.com/StuHelper/stuhelper-platform => ./.tools/candidates/platform

replace github.com/StuHelper/stuhelper-qq-binding => ./.tools/candidates/qq-binding

replace github.com/StuHelper/stuhelper-verification => ./.tools/candidates/verification
