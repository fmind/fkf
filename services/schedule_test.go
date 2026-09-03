package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

type scheduleRunner struct {
	calls []sources.Command
}

func (runner *scheduleRunner) Run(_ context.Context, command sources.Command) (string, error) {
	runner.calls = append(runner.calls, command)
	return "", nil
}

func TestScheduleLinuxInstallStatusDryRunAndRemove(t *testing.T) {
	base, request, runner := linuxScheduleFixture(t)
	assertLinuxScheduleDryRun(t, base, request, runner)
	installed := assertLinuxScheduleInstall(t, base, request, runner)
	assertLinuxScheduleStatus(t, base, request, runner)
	assertLinuxScheduleRemove(t, base, request, runner, installed.Name)
}

func linuxScheduleFixture(t *testing.T) (string, ScheduleRequest, *scheduleRunner) {
	t.Helper()
	home := t.TempDir()
	base := filepath.Join(t.TempDir(), "base with spaces")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &scheduleRunner{}
	return base, ScheduleRequest{
		Action: ScheduleInstall, Home: home, Platform: "linux", Executable: executable,
		Path: "/usr/bin:/bin:relative", Runner: runner,
	}, runner
}

func assertLinuxScheduleDryRun(
	t *testing.T, base string, request ScheduleRequest, runner *scheduleRunner,
) {
	t.Helper()
	request.DryRun = true
	dry, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || !dry.Changed || !dry.Complete || dry.Installed || len(runner.calls) != 0 {
		t.Fatalf("dry install = %#v with %d commands", dry, len(runner.calls))
	}
	for _, file := range dry.Files {
		if _, err := os.Stat(file.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry install wrote %s: %v", file.Path, err)
		}
	}
}

func assertLinuxScheduleInstall(
	t *testing.T, base string, request ScheduleRequest, runner *scheduleRunner,
) *ScheduleReport {
	t.Helper()
	installed, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Installed || !installed.Current || !installed.Changed || !installed.Complete || len(installed.Files) != 2 {
		t.Fatalf("installed report = %#v", installed)
	}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[0].Argv, []string{"systemctl", "--user", "daemon-reload"}) ||
		!reflect.DeepEqual(runner.calls[1].Argv, []string{"systemctl", "--user", "enable", "--now", installed.Name + ".timer"}) {
		t.Fatalf("systemd activation calls = %#v", runner.calls)
	}
	for _, command := range runner.calls {
		if command.Env["HOME"] != request.Home || command.Env["PATH"] != "/usr/bin:/bin" {
			t.Fatalf("manager environment = %#v, want explicit closed HOME/PATH", command.Env)
		}
	}

	service := readScheduleFile(t, installed.Files[0].Path)
	timer := readScheduleFile(t, installed.Files[1].Path)
	for _, want := range []string{
		`Environment="HOME=` + request.Home + `"`, `Environment="PATH=/usr/bin:/bin"`,
		request.Executable, base, `"sync" "--if-due"`, `"build" "--if-stale"`,
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("service omits %q:\n%s", want, service)
		}
	}
	for _, want := range []string{"OnCalendar=hourly", "Persistent=true", "Unit=" + installed.Name + ".service"} {
		if !strings.Contains(timer, want) {
			t.Fatalf("timer omits %q:\n%s", want, timer)
		}
	}
	return installed
}

func assertLinuxScheduleStatus(
	t *testing.T, base string, request ScheduleRequest, runner *scheduleRunner,
) {
	t.Helper()
	runner.calls = nil
	request.Action = ScheduleStatus
	status, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Active || !status.Current || status.Changed || !status.Complete || len(runner.calls) != 2 {
		t.Fatalf("status = %#v with %d commands", status, len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[0].Argv,
		[]string{"systemctl", "--user", "is-enabled", "--quiet", status.Name + ".timer"}) ||
		!reflect.DeepEqual(runner.calls[1].Argv,
			[]string{"systemctl", "--user", "is-active", "--quiet", status.Name + ".timer"}) {
		t.Fatalf("systemd status calls = %#v", runner.calls)
	}
}

func assertLinuxScheduleRemove(
	t *testing.T, base string, request ScheduleRequest, runner *scheduleRunner, name string,
) {
	t.Helper()
	runner.calls = nil
	request.Action = ScheduleRemove
	request.DryRun = true
	removed, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !removed.DryRun || !removed.Changed || !removed.Installed || !removed.Active || len(runner.calls) != 2 {
		t.Fatalf("dry remove = %#v with %d commands", removed, len(runner.calls))
	}
	runner.calls = nil
	request.DryRun = false
	removed, err = Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Installed || !removed.Changed || !removed.Complete {
		t.Fatalf("removed report = %#v", removed)
	}
	if len(runner.calls) != 4 || !reflect.DeepEqual(runner.calls[2].Argv,
		[]string{"systemctl", "--user", "disable", "--now", name + ".timer"}) ||
		!reflect.DeepEqual(runner.calls[3].Argv, []string{"systemctl", "--user", "daemon-reload"}) {
		t.Fatalf("systemd removal calls = %#v", runner.calls)
	}
	for _, file := range removed.Files {
		if _, err := os.Stat(file.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove left %s: %v", file.Path, err)
		}
	}
}

func TestScheduleDarwinInstallsManagedHourlyLaunchAgent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home & owner")
	base := filepath.Join(t.TempDir(), "base & evidence")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &scheduleRunner{}
	request := ScheduleRequest{
		Action: ScheduleInstall, Home: home, Platform: "darwin", UID: 501,
		Executable: executable, Path: "/opt/homebrew/bin:/usr/bin:/bin", Runner: runner,
	}
	report, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Installed || !report.Current || !report.Changed || len(report.Files) != 1 {
		t.Fatalf("launchd install = %#v", report)
	}
	plist := readScheduleFile(t, report.Files[0].Path)
	for _, want := range []string{
		"<key>StartInterval</key>\n  <integer>3600</integer>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>HOME</key>", "home &amp; owner", "<key>PATH</key>",
		`&#34;$1&#34; --base &#34;$2&#34; sync --if-due &amp;&amp; exec &#34;$1&#34; --base &#34;$2&#34; build --if-stale`,
		"base &amp; evidence",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("launch agent omits %q:\n%s", want, plist)
		}
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].Argv,
		[]string{"launchctl", "bootstrap", "gui/501", report.Files[0].Path}) {
		t.Fatalf("launchd activation calls = %#v", runner.calls)
	}
}

func TestScheduleRejectsIncompleteOrUnsupportedRuntimeContext(t *testing.T) {
	base := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []ScheduleRequest{
		{Action: "unknown", Home: t.TempDir(), Platform: "linux", Executable: executable, Path: "/bin"},
		{Action: ScheduleInstall, Home: "", Platform: "linux", Executable: executable, Path: "/bin"},
		{Action: ScheduleInstall, Home: "relative", Platform: "linux", Executable: executable, Path: "/bin"},
		{Action: ScheduleInstall, Home: t.TempDir(), Platform: "windows", Executable: executable, Path: "/bin"},
		{Action: ScheduleInstall, Home: t.TempDir(), Platform: "linux", Executable: "relative", Path: "/bin"},
		{Action: ScheduleInstall, Home: t.TempDir(), Platform: "linux", Executable: executable, Path: "relative"},
	}
	for _, request := range tests {
		if _, err := Schedule(t.Context(), base, request); err == nil {
			t.Fatalf("Schedule(%#v) unexpectedly succeeded", request)
		}
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	request := ScheduleRequest{
		Action: ScheduleInstall, Home: t.TempDir(), Platform: "linux",
		Executable: executable, Path: "/bin", Runner: &scheduleRunner{},
	}
	if _, err := Schedule(canceled, base, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled schedule error = %v, want context.Canceled", err)
	}
}

func TestScheduleInspectionRejectsUnsafeOrUnboundedManagedFiles(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		want    error
	}{
		{
			name: "symlink",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("outside"), core.BaseFileMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Skipf("symlinks are unavailable: %v", err)
				}
			},
			want: core.ErrUnsafePath,
		},
		{
			name: "fifo",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, uint32(core.BaseFileMode)); err != nil {
					t.Skipf("FIFOs are unavailable: %v", err)
				}
			},
			want: core.ErrUnsafePath,
		},
		{
			name: "oversize",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				body := make([]byte, core.MaxControlFileBytes+1)
				if err := os.WriteFile(path, body, core.BaseFileMode); err != nil {
					t.Fatal(err)
				}
			},
			want: core.ErrFileTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, request, _ := linuxScheduleFixture(t)
			request.Action = ScheduleStatus
			plan, err := planSchedule(base, request)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(plan.files[0].path), core.BaseDirMode); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, plan.files[0].path)
			if _, err := Schedule(t.Context(), base, request); !errors.Is(err, test.want) {
				t.Fatalf("Schedule(status) error = %v, want %v", err, test.want)
			}
		})
	}
}

type activationRetryRunner struct {
	active             bool
	activationAttempts int
}

func (runner *activationRetryRunner) Run(_ context.Context, command sources.Command) (string, error) {
	display := strings.Join(command.Argv, " ")
	if strings.Contains(display, " is-enabled ") || strings.Contains(display, " is-active ") {
		if !runner.active {
			return "", errors.New("timer is not active")
		}
		return "", nil
	}
	if strings.Contains(display, " enable --now ") {
		runner.activationAttempts++
		if runner.activationAttempts == 1 {
			return "", errors.New("activation failed")
		}
		runner.active = true
	}
	return "", nil
}

func TestScheduleInstallRetriesActivationWhenManagedFilesAreAlreadyCurrent(t *testing.T) {
	base, request, _ := linuxScheduleFixture(t)
	runner := &activationRetryRunner{}
	request.Runner = runner
	if _, err := Schedule(t.Context(), base, request); err == nil {
		t.Fatal("first install unexpectedly hid the activation failure")
	}
	report, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || !report.Active || !report.Current || runner.activationAttempts != 2 {
		t.Fatalf("retry report=%+v attempts=%d; want manager activation retried", report, runner.activationAttempts)
	}
}

func TestScheduleRemoveDeactivatesAnOrphanedManagerUnit(t *testing.T) {
	base, request, runner := linuxScheduleFixture(t)
	request.Action = ScheduleRemove
	request.Runner = runner
	report, err := Schedule(t.Context(), base, request)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.Installed || report.Active || !report.Complete {
		t.Fatalf("orphan removal report = %+v", report)
	}
	if len(runner.calls) != 4 || !reflect.DeepEqual(runner.calls[0].Argv,
		[]string{"systemctl", "--user", "is-enabled", "--quiet", report.Name + ".timer"}) ||
		!reflect.DeepEqual(runner.calls[1].Argv,
			[]string{"systemctl", "--user", "is-active", "--quiet", report.Name + ".timer"}) ||
		!reflect.DeepEqual(runner.calls[2].Argv,
			[]string{"systemctl", "--user", "disable", "--now", report.Name + ".timer"}) {
		t.Fatalf("orphan manager calls = %#v", runner.calls)
	}
}

func readScheduleFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
