package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/urfave/cli/v3"

	"github.com/fmind/fkf/services"
)

func newScheduleCommand() *cli.Command {
	return &cli.Command{
		Name: "schedule", Category: groupRun,
		Usage: "Install, inspect, or remove this base's hourly user schedule.",
		Description: "Installs one managed systemd user timer on Linux or launchd agent on macOS. " +
			"The unit pins this fkf executable and base, and exports explicit HOME and PATH values.",
		Commands: []*cli.Command{
			newScheduleActionCommand(services.ScheduleInstall, true),
			newScheduleActionCommand(services.ScheduleStatus, false),
			newScheduleActionCommand(services.ScheduleRemove, true),
		},
	}
}

func newScheduleActionCommand(action services.ScheduleAction, supportsDryRun bool) *cli.Command {
	flags := []cli.Flag{}
	if supportsDryRun {
		flags = append(flags, &cli.BoolFlag{Name: "dry-run", Usage: "Print the managed change without writing or invoking the user scheduler."})
	}
	return &cli.Command{
		Name: string(action), Aliases: []string{string(action)[0:1]}, Usage: scheduleActionUsage(action), Flags: flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := requireNoArgs(cmd, ""); err != nil {
				return err
			}
			base, err := openBase(cmd)
			if err != nil {
				return err
			}
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate current FKF executable: %w", err)
			}
			run := func() error {
				report, err := services.Schedule(ctx, base.Root(), services.ScheduleRequest{
					Action: action, Home: os.Getenv("HOME"), Path: os.Getenv("PATH"),
					Platform: runtime.GOOS, Executable: executable, UID: os.Getuid(), DryRun: cmd.Bool("dry-run"),
				})
				return emitSchedule(cmd, report, err)
			}
			if action == services.ScheduleStatus || cmd.Bool("dry-run") {
				return run()
			}
			return withWriterLock(ctx, base.Root(), run)
		},
	}
}

func scheduleActionUsage(action services.ScheduleAction) string {
	switch action {
	case services.ScheduleInstall:
		return "Install and activate the managed hourly user schedule." + markWrite
	case services.ScheduleStatus:
		return "Report whether the managed hourly user schedule is installed and current."
	default:
		return "Deactivate and remove the managed hourly user schedule." + markWrite
	}
}

func emitSchedule(cmd *cli.Command, report *services.ScheduleReport, err error) error {
	if err != nil || cmd.Root().String("format") != formatText {
		return emit(cmd, report, err)
	}
	state := "missing"
	switch {
	case report.Current:
		state = "current"
	case report.Active && !report.Installed:
		state = "active-with-missing-files"
	case report.Installed:
		state = "drifted"
	}
	prefix := "schedule"
	if report.DryRun {
		prefix = "schedule dry-run"
	}
	if _, err := fmt.Fprintf(cmd.Root().Writer, "%s %s %s: %s\n", prefix, report.Platform, report.Name, state); err != nil {
		return err
	}
	for _, file := range report.Files {
		if _, err := fmt.Fprintf(cmd.Root().Writer, "%s: %s\n", file.State, file.Path); err != nil {
			return err
		}
	}
	return nil
}
