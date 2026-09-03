package checks_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGcloudAuthReadyRequiresANonemptyActiveAccount(t *testing.T) {
	helper := filepath.Join(repositoryRoot(t), "presets", "bin", "gcloud-auth-ready.sh")
	fakeBin := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	fake := filepath.Join(fakeBin, "gcloud")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$GCLOUD_TEST_ARGS"
[ "${GCLOUD_TEST_FAIL:-}" != 1 ] || exit 7
printf '%s' "${GCLOUD_TEST_ACCOUNT:-}"
`), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, account, fail string
		wantReady           bool
	}{
		{name: "active account", account: "owner@example.test", wantReady: true},
		{name: "empty account"},
		{name: "provider failure", fail: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(helper)
			command.Env = []string{
				"PATH=" + fakeBin,
				"GCLOUD_TEST_ARGS=" + argsPath,
				"GCLOUD_TEST_ACCOUNT=" + test.account,
				"GCLOUD_TEST_FAIL=" + test.fail,
			}
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if (err == nil) != test.wantReady {
				t.Fatalf("gcloud-auth-ready error = %v, want ready %t", err, test.wantReady)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("probe disclosed output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			args, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "auth\nlist\n--filter=status:ACTIVE\n--format=value(account)"
			if strings.TrimSpace(string(args)) != want {
				t.Fatalf("gcloud arguments = %q, want %q", strings.TrimSpace(string(args)), want)
			}
		})
	}
}
