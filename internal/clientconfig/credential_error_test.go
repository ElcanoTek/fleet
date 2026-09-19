package clientconfig

import (
	"strings"
	"testing"
)

func TestCatalogValidationDoesNotEchoCredentialFields(t *testing.T) {
	const bad = "fake pasted credential: do-not-log"
	for _, e := range []RemoteMCPCatalogEntry{
		{Name: "demo", Auth: "api_key", APIKeyHeader: bad},
		{Name: "demo", Auth: "api_key", APIKeyQuery: bad},
	} {
		err := validateRemoteMCPEntryMeta(&e)
		if err == nil {
			t.Fatal("invalid field accepted")
		}
		if strings.Contains(err.Error(), bad) {
			t.Fatal("raw credential field leaked")
		}
	}
}
