// The Doory-compatible AI contract, embedded verbatim (byte drift from the
// upstream mirror is caught by TestContractBytesMatchesMirror).
package doory

import (
	_ "embed"
)

//go:embed contract_openapi.yaml
var contractYAML []byte
