package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestBuildAndSyncTextNameTheLexicalIndexTheyRebuilt(t *testing.T) {
	rebuilt := &services.LexicalIndexBuild{
		URI: services.LexicalIndexPath, Entries: 7, Postings: 31, Bytes: 509,
		Elapsed: "4ms", Mode: "full",
	}
	for name, render := range map[string]func(*textWriter){
		"build": func(w *textWriter) { writeBuildText(w, &services.BuildReport{Index: rebuilt}) },
		"sync":  func(w *textWriter) { writeSyncText(w, &services.SyncReport{Index: rebuilt}) },
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			render(&textWriter{out: &output})
			for _, want := range []string{services.LexicalIndexPath, "7 entries", "31 postings", "509 bytes"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("output = %q, want %q", output.String(), want)
				}
			}
		})
	}
}

func TestFindTextNamesUsedAndFallbackLexicalPaths(t *testing.T) {
	for _, test := range []struct {
		name   string
		result *services.FindResult
		want   string
	}{
		{
			name: "used listing",
			result: &services.FindResult{
				Index: &services.LexicalIndexUse{Path: services.LexicalIndexPath, Used: true},
			},
			want: "index index/.fkf-index.tsv used\n",
		},
		{
			name: "fallback count",
			result: &services.FindResult{
				Volumes: []services.DayVolume{{Date: "2026-05-09"}},
				Index: &services.LexicalIndexUse{
					Path: services.LexicalIndexPath, Reason: services.LexicalIndexFallbackStale,
				},
			},
			want: "index index/.fkf-index.tsv fallback=stale\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			writeFindText(&textWriter{out: &output}, test.result)
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestBuildHelpSaysBareBuildIncludesTheLexicalIndex(t *testing.T) {
	usage := newBuildCommand().UsageText
	for _, want := range []string{"fkf build", "wiki, graph, and lexical index"} {
		if !strings.Contains(usage, want) {
			t.Fatalf("build usage = %q, want %q", usage, want)
		}
	}
}
