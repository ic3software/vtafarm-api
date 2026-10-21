package setup

import (
	"strings"
	"testing"
)

func TestRegistryJobFailureRetainsUsefulDiagnosticsWithoutRawLogs(t *testing.T) {
	const diagnostic = "could not sign the DID-management task with this VTA's operational key"
	for _, output := range []string{
		"Error: " + diagnostic + "\nprivateKeyMultibase=test-key-never-persist",
		"privateKeyMultibase=test-key-never-persist",
		"",
	} {
		failure := registryJobFailure("identity", "Job has reached the specified backoff limit", output).Error()
		if !strings.Contains(failure, "identity job failed") || strings.Contains(failure, "test-key-never-persist") || strings.Contains(failure, "check this setup job's logs") {
			t.Fatal("failure summary leaked raw logs or required a deleted Pod")
		}
		if strings.Contains(output, diagnostic) && !strings.Contains(failure, diagnostic) {
			t.Fatal("failure summary lost the known signing error")
		}
	}
}
