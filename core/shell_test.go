package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Keep interpreter-backed isolation tests bounded without turning cold startup on a loaded CI
// runner into a latency contract.
const interpreterStartupTestTimeout = 5 * time.Second

func TestCommandEnvironmentIsContextScoped(t *testing.T) {
	ctx, err := WithCommandEnvironment(context.Background(), map[string]string{"FKF_SYNTHETIC_ACCOUNT": "isolated"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := RunCLI(ctx, []string{"sh", "-c", `printf '%s' "$FKF_SYNTHETIC_ACCOUNT"`}, "", time.Second)
	if err != nil || output != "isolated" {
		t.Fatalf("scoped environment output = %q, %v", output, err)
	}
	plain, err := RunCLI(context.Background(), []string{"sh", "-c", `printf '%s' "${FKF_SYNTHETIC_ACCOUNT:-}"`}, "", time.Second)
	if err != nil || strings.TrimSpace(plain) != "" {
		t.Fatalf("scoped environment leaked to process = %q, %v", plain, err)
	}
}

func TestRunCLIInheritsProviderConfigurationFromTheLaunchingProcess(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", filepath.Join(t.TempDir(), "gh"))
	output, err := RunCLI(t.Context(), []string{"sh", "-c", `printf '%s' "$GH_CONFIG_DIR"`}, "", time.Second)
	if err != nil || output != os.Getenv("GH_CONFIG_DIR") {
		t.Fatalf("inherited provider configuration = %q, %v; want the launching process value", output, err)
	}
}

func TestRunCLIRemovesRuntimeStartupLoaders(t *testing.T) {
	const decoy = "ENV=retained LD_PRELOAD=retained"
	t.Setenv("FKF_STARTUP_LOADER_DECOY", decoy)
	for _, key := range []string{
		"BASH_ENV", "ENV", "ZDOTDIR", "fish_function_path",
		"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONINSPECT",
		"NODE_OPTIONS", "NODE_PATH", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH",
		"DYLD_FALLBACK_LIBRARY_PATH", "GCONV_PATH", "LUA_INIT", "R_PROFILE_USER",
	} {
		t.Setenv(key, "wiki/startup")
	}
	output, err := RunCLI(t.Context(), []string{"sh", "-c", "env"}, t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	childValues := map[string]string{}
	for line := range strings.SplitSeq(output, "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			childValues[key] = value
		}
	}
	if childValues["FKF_STARTUP_LOADER_DECOY"] != decoy {
		t.Fatalf("unrelated child environment value = %q, want %q", childValues["FKF_STARTUP_LOADER_DECOY"], decoy)
	}
	for _, key := range []string{
		"BASH_ENV", "ENV", "ZDOTDIR", "fish_function_path",
		"PYTHONPATH", "NODE_OPTIONS", "PERL5OPT", "RUBYOPT", "JAVA_TOOL_OPTIONS",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GCONV_PATH", "LUA_INIT", "R_PROFILE_USER",
	} {
		if _, found := childValues[key]; found {
			t.Errorf("child environment retains runtime startup loader %s", key)
		}
	}
}

func TestRunCLIRemovesConfigRootsThatCouldResolveInsideTheCommandDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "relative", value: "wiki"},
		{name: "absolute", value: filepath.Join(root, "wiki")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", test.value)
			output, err := RunCLI(t.Context(), []string{"sh", "-c", `printf '%s' "${XDG_CONFIG_HOME-unset}"`}, root, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if output != "unset" {
				t.Fatalf("XDG_CONFIG_HOME = %q, want unsafe command-directory path removed", output)
			}
		})
	}

	t.Setenv("XDG_CONFIG_HOME", outside)
	output, err := RunCLI(t.Context(), []string{"sh", "-c", `printf '%s' "$XDG_CONFIG_HOME"`}, root, time.Second)
	if err != nil || output != outside {
		t.Fatalf("safe XDG_CONFIG_HOME = %q, %v; want provider configuration preserved", output, err)
	}
}

func TestRunCLIDoesNotImportPythonFromTheCommandDirectory(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 is not installed: %v", err)
	}
	root := t.TempDir()
	wiki := filepath.Join(root, "wiki")
	if err := os.Mkdir(wiki, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wiki, "sitecustomize.py"), []byte("print('sourced')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", "wiki")
	output, err := RunCLI(t.Context(), []string{python, "-c", "print('declared')"}, root, interpreterStartupTestTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if output != "declared\n" {
		t.Fatalf("Python output = %q; mutable authored startup code ran before the declared command", output)
	}
}

func TestRunCLIBoundedLimitsStdoutAndStderrIndependently(t *testing.T) {
	for _, script := range []string{`printf '123456789'`, `printf '123456789' >&2`} {
		_, err := RunCLIBounded(context.Background(), []string{"sh", "-c", script}, "", "", time.Second, 8)
		if !errors.Is(err, ErrCLIOutputTooLarge) {
			t.Fatalf("RunCLIBounded(%q) error = %v", script, err)
		}
	}
}

// TestRunCLIRejectsARelativeSlashExecutableAcrossWorkingDirectories keeps resolution and exec
// on one interpretation. A relative argv[0] used to be statted from fkf's process directory,
// then executed after Cmd.Dir changed to the base: the file approved and the file run could
// therefore differ. Base helpers are bare names from the trusted PATH; an explicit path must
// be absolute.
func TestRunCLIRejectsARelativeSlashExecutableAcrossWorkingDirectories(t *testing.T) {
	processDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf reached"), 0o700); err != nil {
		t.Fatal(err)
	}
	commandDirectory := t.TempDir()
	output, err := RunCLI(t.Context(), []string{executable}, commandDirectory, time.Second)
	if err != nil || output != "reached" {
		t.Fatalf("RunCLI(absolute executable) = %q, %v; want the explicit path to run", output, err)
	}
	relative, err := filepath.Rel(processDirectory, executable)
	if err != nil || !strings.ContainsRune(relative, os.PathSeparator) {
		t.Fatalf("relative executable = %q, %v; want a slash-bearing path", relative, err)
	}

	_, err = RunCLI(t.Context(), []string{relative}, commandDirectory, time.Second)
	if err == nil || !strings.Contains(err.Error(), "relative executable") {
		t.Fatalf("RunCLI() error = %v, want a deliberate relative-executable refusal", err)
	}
}

// TestRunCLIRemovesRelativePATHEntriesFromTheChild closes the shell half of executable
// resolution. ResolveExecutable has always ignored relative entries, but passing the original
// PATH to the resolved shell let `sh -c helper` search the command directory through an empty,
// dot, or relative entry after review had explicitly ignored it.
func TestRunCLIRemovesRelativePATHEntriesFromTheChild(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"", ".", "relative-bin"} {
		t.Run(fmt.Sprintf("entry_%q", entry), func(t *testing.T) {
			commandDirectory := t.TempDir()
			helperDirectory := commandDirectory
			if entry == "relative-bin" {
				helperDirectory = filepath.Join(commandDirectory, entry)
				if err := os.Mkdir(helperDirectory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(helperDirectory, "helper"),
				[]byte("#!/bin/sh\nprintf reached\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			pathValue := strings.Join([]string{entry, filepath.Dir(shell)}, string(os.PathListSeparator))
			ctx, err := WithCommandEnvironment(t.Context(), map[string]string{"PATH": pathValue})
			if err != nil {
				t.Fatal(err)
			}
			output, err := RunCLI(ctx, []string{"sh", "-c", "helper"}, commandDirectory, time.Second)
			if err == nil || output != "" {
				t.Fatalf("RunCLI() = %q, %v; relative PATH entry %q reached an unreviewed helper", output, err, entry)
			}
		})
	}
}

// TestRunCLIStdinFeedsTheDocument covers the one caller that needs it: a `?jq=` URI hands a
// stored document to jq on stdin, so the expression stays one argv element with no shell.
func TestRunCLIStdinFeedsTheDocument(t *testing.T) {
	output, err := RunCLIStdin(context.Background(), []string{"sh", "-c", "cat"}, "", `{"a":1}`, time.Second)
	if err != nil {
		t.Fatalf("RunCLIStdin() error = %v", err)
	}
	if output != `{"a":1}` {
		t.Fatalf("RunCLIStdin() = %q, want the document echoed back", output)
	}
}

// TestRunCLIFailureKeepsProviderStderrPrivate pins the provider boundary: stderr is useful for
// a declared retry decision, but it is provider data and must never become a terminal message,
// structured log, or serialized error. This core boundary exposes only the exit class; the
// source runner may attach the separately reviewed run: context without exposing stderr.
func TestRunCLIFailureKeepsProviderStderrPrivate(t *testing.T) {
	const privateStderr = "synthetic-provider-private-stderr"
	t.Setenv("FKF_SYNTHETIC_PRIVATE_STDERR", privateStderr)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := RunCLI(context.Background(),
		[]string{"sh", "-c", `printf '%s' "$FKF_SYNTHETIC_PRIVATE_STDERR" >&2; exit 3`}, "", time.Second)
	if err == nil {
		t.Fatal("RunCLI() succeeded, want a failure")
	}
	encoded, encodeErr := json.Marshal(err)
	if encodeErr != nil {
		t.Fatalf("marshal the command failure: %v", encodeErr)
	}
	for name, diagnostic := range map[string]string{
		"error": err.Error(), "debug": fmt.Sprintf("%#v", err),
		"log": logs.String(), "JSON": string(encoded),
	} {
		if strings.Contains(diagnostic, privateStderr) {
			t.Fatalf("%s leaked provider stderr: %q", name, diagnostic)
		}
	}
	if !strings.Contains(err.Error(), "status 3") {
		t.Fatalf("RunCLI() error = %v, want the safe exit status", err)
	}
	for _, want := range []string{`status=exit`, `diagnostic="command exited with status 3"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("command log = %q, want the safe field %q", logs.String(), want)
		}
	}
	if len(err.Error()) > 128 {
		t.Fatalf("RunCLI() diagnostic is unexpectedly unbounded: %d bytes", len(err.Error()))
	}
}

// TestRunCLIHonoursCancellation confirms an interrupt cancels an in-flight subprocess rather
// than waiting out the per-call timeout, which is what makes Ctrl-C during a sync immediate.
func TestRunCLIHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunCLI(ctx, []string{"sh", "-c", "sleep 30"}, "", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCLI() error = %v, want context.Canceled", err)
	}
}

// TestRunCLITimeoutBoundsWallClock is the regression test for a timeout that bounded nothing.
// `sh -c 'sleep 30 | cat'` forks a pipeline; killing only the shell left `sleep` holding the
// stdout pipe, so Wait blocked for the full 30s while the caller had asked for one. The
// assertion is wall-clock on purpose: the previous test asserted only that the configured
// duration reached the Command struct, which stayed true the whole time the behaviour was broken.
func TestRunCLITimeoutBoundsWallClock(t *testing.T) {
	started := time.Now()
	_, err := RunCLI(context.Background(), []string{"sh", "-c", "sleep 30 | cat"}, "", 500*time.Millisecond)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunCLI() error = %v, want context.DeadlineExceeded", err)
	}
	// Generous against a loaded CI runner, but far below the 30s the pipeline would otherwise
	// take: the point is that the deadline, not the sleep, ended the call.
	if elapsed > 10*time.Second {
		t.Fatalf("RunCLI() took %s for a 500ms timeout; the deadline did not bound the pipeline", elapsed)
	}
}

// TestRunCLIFailureNeverCarriesStdout keeps the collected payload out of the error and the log.
// Stdout is what a `run:` line prints — the mail, tickets, and shell history the base exists to
// keep local — so a failing source must not turn it into a terminal message or a log line.
func TestRunCLIFailureNeverCarriesStdout(t *testing.T) {
	// The payload is produced by the command rather than written in it, so finding it in the
	// diagnostic can only mean stdout was copied there — the command string legitimately
	// appears in the error and would otherwise match.
	//
	// This test is about the failure envelope, not timeout precision; the dedicated wall-clock
	// test above covers that contract. Race instrumentation plus parallel package tests can
	// delay even this tiny child past one second before it is scheduled, so retain a generous
	// finite guard without turning host load into a different error class.
	_, err := RunCLI(context.Background(),
		[]string{"sh", "-c", `printf 'sensitive-payload' | tr 'a-z' 'A-Z'; exit 7`}, "", 10*time.Second)
	if err == nil {
		t.Fatal("RunCLI() succeeded, want a failure")
	}
	if strings.Contains(err.Error(), "SENSITIVE-PAYLOAD") {
		t.Fatalf("RunCLI() error = %v, want stdout kept out of the diagnostic", err)
	}
	// The exit status is the one fact an operator needs, and it used to be dropped whenever
	// the command had printed anything at all.
	if !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("RunCLI() error = %v, want it to report the exit status", err)
	}
}
