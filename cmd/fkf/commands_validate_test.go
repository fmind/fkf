package main

import "testing"

func TestValidateExposesCollectedRecordTitleChecks(t *testing.T) {
	command := newValidateCommand()
	records := command.Command("records")
	if records == nil {
		t.Fatal("validate records subcommand is missing")
	}
	if records.Action == nil {
		t.Fatal("validate records has no action")
	}
}
