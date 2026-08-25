package services_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

const (
	controlBegin       = "# >>> fkf managed block — do not edit between the markers"
	controlBeginPrefix = "# >>> fkf managed block"
	controlEnd         = "# <<< fkf managed block"
)

func TestEnsureManagedBlockRejectsMalformedTopologyWithoutWriting(t *testing.T) {
	malformed := map[string]struct {
		body string
		want string
	}{
		"truncated begin": {
			body: "owner-before\n" + controlBegin + "\ngenerated-without-an-end\nowner-after\n",
			want: "no matching end marker",
		},
		"duplicate begin": {
			body: controlBegin + "\n" + controlBegin + "\n" + controlEnd + "\n",
			want: "more than one canonical begin marker",
		},
		"duplicate end": {
			body: controlBegin + "\n" + controlEnd + "\n" + controlEnd + "\n",
			want: "more than one canonical end marker",
		},
		"noncanonical begin": {
			body: controlBeginPrefix + " — obsolete spelling\n" + controlEnd + "\n",
			want: "non-canonical begin marker",
		},
		"noncanonical end": {
			body: controlBegin + "\n" + controlEnd + " — obsolete spelling\n",
			want: "non-canonical end marker",
		},
		"orphan end": {
			body: "owner-before\n" + controlEnd + "\nowner-after\n",
			want: "no matching begin marker",
		},
		"end before begin": {
			body: controlEnd + "\n" + controlBegin + "\n",
			want: "before it",
		},
		"duplicate pair": {
			body: controlBegin + "\na\n" + controlEnd + "\n" + controlBegin + "\nb\n" + controlEnd + "\n",
			want: "more than one canonical begin marker",
		},
	}
	files := map[string]func() string{
		".gitignore":     func() string { return services.ManagedIgnoreBlock(false) },
		".gitattributes": services.ManagedAttributesBlock,
	}
	for file, block := range files {
		for name, test := range malformed {
			t.Run(file+"/"+name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), file)
				if err := os.WriteFile(path, []byte(test.body), core.BaseFileMode); err != nil {
					t.Fatal(err)
				}
				before := mustRead(t, path)

				changed, err := services.EnsureManagedBlock(path, block())
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("EnsureManagedBlock() = changed %t, error %v; want %q", changed, err, test.want)
				}
				if changed {
					t.Fatal("a rejected managed block was reported changed")
				}
				if after := mustRead(t, path); after != before {
					t.Fatalf("rejected refresh changed %s:\n%s", file, after)
				}
			})
		}
	}
}

func TestEnsureManagedBlockBoundsControlFileReads(t *testing.T) {
	for file, block := range map[string]string{
		".gitignore":     services.ManagedIgnoreBlock(false),
		".gitattributes": services.ManagedAttributesBlock(),
	} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), file)
			body := strings.Repeat("x", int(core.MaxControlFileBytes)+1)
			if err := os.WriteFile(path, []byte(body), core.BaseFileMode); err != nil {
				t.Fatal(err)
			}

			changed, err := services.EnsureManagedBlock(path, block)
			if changed || !errors.Is(err, core.ErrFileTooLarge) {
				t.Fatalf("EnsureManagedBlock() = changed %t, error %v; want the control-file bound", changed, err)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Size() != core.MaxControlFileBytes+1 {
				t.Fatalf("rejected oversized file changed: size=%d", info.Size())
			}
		})
	}
}

func TestInitPreflightsBothManagedFilesBeforeRefreshingEither(t *testing.T) {
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	request := services.InitRequest{Path: root, Preset: services.PresetPersonal, SkipGit: true}
	if _, err := services.Init(t.Context(), request, clock); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(root, ".gitignore")
	attributesPath := filepath.Join(root, ".gitattributes")
	staleIgnore := strings.Replace(mustRead(t, ignorePath), ".agents/tmp/", ".agents/obsolete/", 1)
	brokenAttributes := mustRead(t, attributesPath) + controlEnd + "\n"
	if err := os.WriteFile(ignorePath, []byte(staleIgnore), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attributesPath, []byte(brokenAttributes), core.BaseFileMode); err != nil {
		t.Fatal(err)
	}

	if _, err := services.Init(t.Context(), request, clock); err == nil || !strings.Contains(err.Error(), "end marker") {
		t.Fatalf("Init() error = %v, want malformed .gitattributes rejected", err)
	}
	if got := mustRead(t, ignorePath); got != staleIgnore {
		t.Fatalf("refresh rewrote .gitignore before preflighting .gitattributes:\n%s", got)
	}
	if got := mustRead(t, attributesPath); got != brokenAttributes {
		t.Fatalf("refresh rewrote malformed .gitattributes:\n%s", got)
	}
}
