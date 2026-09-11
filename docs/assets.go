package docs

import _ "embed"

// OpenAPI is served by the authenticated /openapi.yaml endpoint.
//
//go:embed openapi.yaml
var OpenAPI []byte
