package services_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func BenchmarkLexicalIndexContext100K(b *testing.B) {
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	b.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	b.Setenv(core.BaseEnvVar, "")
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, core.ConfigFileName), []byte(baseConfig), core.BaseFileMode); err != nil {
		b.Fatal(err)
	}
	base, err := services.Open(root)
	if err != nil {
		b.Fatal(err)
	}
	base.Now = func() time.Time { return testClock }
	source := base.Config.Sources["synthetic"]
	records := make([]sources.Record, 100_000)
	for index := range records {
		title := fmt.Sprintf("Routine evidence %06d", index)
		if index == len(records)-1 {
			title = "needle-99999"
		}
		records[index] = sources.Record{
			"id": fmt.Sprintf("record-%06d", index), "t": "2026-05-09T09:00:00Z", "subject": title,
		}
	}
	document := &sources.Document{
		FKF: sources.SchemaVersion, Source: source.Name, Layer: source.Layer, Date: "2026-05-09",
		WindowStart: "2026-05-09T00:00:00Z", WindowEnd: "2026-05-10T00:00:00Z",
		CollectedAt: testClock.Format(time.RFC3339), Schema: sources.SchemaOf(source),
		Fields: sources.FieldsOf(source), Body: source.HasBody(), Count: len(records), Records: records,
	}
	if err := base.WriteDocument(document); err != nil {
		b.Fatal(err)
	}
	if _, err := services.BuildLexicalIndex(b.Context(), base); err != nil {
		b.Fatal(err)
	}
	request := services.ContextRequest{Query: "needle-99999", Budget: 4096}
	if pack, err := services.BuildContext(b.Context(), base, request); err != nil || !pack.Receipt.Index.Used {
		b.Fatalf("indexed warmup = %+v, %v", pack, err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		pack, err := services.BuildContext(b.Context(), base, request)
		if err != nil {
			b.Fatal(err)
		}
		if !pack.Receipt.Index.Used || len(pack.Items) != 1 {
			b.Fatalf("indexed pack = %+v", pack)
		}
	}
}
