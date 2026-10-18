package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runHelper re-runs this test binary with VELOX_CLI_HELPER set, so a test
// can watch a subcommand exit with its code.
func runHelper(t *testing.T, test, helper string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), append(env, "VELOX_CLI_HELPER="+helper)...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// `velox migrate force` is a one-release stub: exit 2 and a pointer.
func TestMigrateForceIsRemoved(t *testing.T) {
	if os.Getenv("VELOX_CLI_HELPER") == "force" {
		runMigrateForce()
		return
	}
	out, code := runHelper(t, "TestMigrateForceIsRemoved", "force")
	if code != 2 {
		t.Fatalf("exit code %d, want 2; output:\n%s", code, out)
	}
	for _, want := range []string{"removed in v0.9.0", "run \"velox migrate\" again", "v0.8.1/docs/ops/runbook/migration-failed-midway.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// `velox migrate rollback` refuses outside APP_ENV=local, before it
// connects anywhere.
func TestMigrateRollbackRefusesOutsideLocal(t *testing.T) {
	if os.Getenv("VELOX_CLI_HELPER") == "rollback" {
		runMigrateRollback()
		return
	}
	for _, env := range []string{"production", " Staging "} {
		out, code := runHelper(t, "TestMigrateRollbackRefusesOutsideLocal", "rollback",
			"APP_ENV="+env, "DATABASE_URL=postgres://u:p@127.0.0.1:1/unreachable")
		if code != 2 || !strings.Contains(out, "runs only with APP_ENV=local") || !strings.Contains(out, "restore the backup") {
			t.Fatalf("APP_ENV=%q: exit %d, output:\n%s", env, code, out)
		}
	}
}
