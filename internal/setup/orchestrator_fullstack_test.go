package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediatorPhase2CommandRedactsLegacyKeyWithoutForcingTTY(t *testing.T) {
	command := fsMediatorPhase2Command("digest-value")

	for _, want := range []string{
		"mkfifo",
		`Private key \(multibase\):`,
		"[REDACTED]",
		"setup_status=$?",
		"wait \"$filter_pid\"",
		"--digest 'digest-value'",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("phase 2 command missing %q", want)
		}
	}
	if strings.Contains(command, "--show-admin-key") {
		t.Fatal("phase 2 command forces the admin key into a terminal")
	}
}

func TestMediatorPhase2CommandFiltersKeyAndPreservesSetupExit(t *testing.T) {
	dir := t.TempDir()
	setup := filepath.Join(dir, "mediator-setup")
	if err := os.WriteFile(setup, []byte("#!/bin/sh\nprintf '\\033[2mPrivate key (multibase): test-secret-never-log\\033[0m\\n'\nprintf 'setup output\\n'\nexit \"${FAKE_SETUP_EXIT:-0}\"\n"), 0o700); err != nil {
		t.Fatalf("write fake mediator-setup: %v", err)
	}

	for _, tt := range []struct {
		name     string
		exitCode string
		wantExit int
	}{
		{name: "success", exitCode: "0", wantExit: 0},
		{name: "failure", exitCode: "7", wantExit: 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", fsMediatorPhase2Command("digest-value"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FAKE_SETUP_EXIT="+tt.exitCode)
			output, err := cmd.CombinedOutput()
			if tt.wantExit == 0 && err != nil {
				t.Fatalf("phase 2 command error = %v", err)
			}
			if tt.wantExit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.wantExit {
					t.Fatalf("phase 2 command error = %v, want exit %d", err, tt.wantExit)
				}
			}
			got := string(output)
			if strings.Contains(got, "test-secret-never-log") {
				t.Fatal("phase 2 command leaked the private key")
			}
			if !strings.Contains(got, "Private key (multibase): [REDACTED]") || !strings.Contains(got, "setup output") {
				t.Fatalf("filtered output = %q", got)
			}
		})
	}
}
