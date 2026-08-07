// Generated contract types as their own Go module.
//
// A separate module rather than part of a service module, because all three Go
// services import these types and none of them owns them. Services depend on it
// with a `replace` directive pointing at this directory, so there is no
// publishing step and no version skew between services — they always build
// against the contracts in the same commit.
module github.com/mhayk/overpass/gen/go

go 1.25

require (
	github.com/getkin/kin-openapi v0.146.0
	github.com/go-chi/chi/v5 v5.3.1
	github.com/oapi-codegen/runtime v1.6.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/go-openapi/jsonpointer v0.22.5 // indirect
	github.com/go-openapi/swag/jsonname v0.25.5 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	golang.org/x/text v0.32.0 // indirect
)
