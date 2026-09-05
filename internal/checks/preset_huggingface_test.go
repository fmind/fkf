package checks_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type huggingFaceRecord struct {
	UID        string `json:"uid"`
	ID         string `json:"id"`
	Type       string `json:"type"`
	Updated    string `json:"updated"`
	Visibility string `json:"visibility"`
	URL        string `json:"url"`
}

func TestHuggingFaceRepositoryHelperUsesTheNativeCLIAndProjectsOnlyCompleteMetadata(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	for _, name := range []string{"cat", "jq", "mktemp", "rm"} {
		linkPresetTool(t, bin, name)
	}
	writePresetFake(t, bin, "hf", `
[ "$*" = "repos ls --limit 10001 --format json" ] || exit 64
case "${HF_CASE:?}" in
  normal) printf '%s\n' '[{"id":"owner/zeta","type":"dataset","updated":"2026-09-03","visibility":"public","storage":"30 bytes"},{"id":"owner/alpha","type":"model","updated":"2026-09-02","visibility":"private","storage":"10 bytes"},{"id":"owner/beta","type":"bucket","updated":"2026-09-01","visibility":"private","storage":"20 bytes"}]' ;;
  empty) printf '%s\n' '[]' ;;
  malformed) printf '%s\n' '{' ;;
  duplicate) printf '%s\n' '[{"id":"owner/a","type":"model"},{"id":"owner/a","type":"model"}]' ;;
  unknown) printf '%s\n' '[{"id":"owner/a","type":"collection"}]' ;;
  exact) jq -nc '[range(0; 10000) | {id:("owner/repo-" + tostring),type:"model",updated:"2026-09-03",visibility:"private"}]' ;;
  over) jq -nc '[range(0; 10001) | {id:("owner/repo-" + tostring),type:"model",updated:"2026-09-03",visibility:"private"}]' ;;
  partial) printf '%s\n' '[{"id":"owner/partial","type":"model"}]'; exit 9 ;;
  *) exit 64 ;;
esac
`)

	stdout, stderr, err := runPresetScript(t, "huggingface-repositories-json.sh", home, bin, []string{"HF_CASE=normal"})
	if err != nil {
		t.Fatalf("normal helper failed: %v stderr=%q", err, stderr)
	}
	var records []huggingFaceRecord
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("decode helper output: %v\n%s", err, stdout)
	}
	if len(records) != 3 || records[0].UID != "model:owner/alpha" || records[1].UID != "bucket:owner/beta" ||
		records[2].UID != "dataset:owner/zeta" || records[1].URL != "https://huggingface.co/buckets/owner/beta" {
		t.Fatalf("projected records = %#v", records)
	}
	if strings.Contains(stdout, "storage") {
		t.Fatalf("helper retained formatted storage as an exact byte count: %s", stdout)
	}

	stdout, stderr, err = runPresetScript(t, "huggingface-repositories-json.sh", home, bin, []string{"HF_CASE=empty"})
	if err != nil || strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("empty helper = stdout %q stderr %q err %v", stdout, stderr, err)
	}

	stdout, stderr, err = runPresetScript(t, "huggingface-repositories-json.sh", home, bin, []string{"HF_CASE=exact"})
	if err != nil {
		t.Fatalf("10,000 records failed: %v stderr=%q", err, stderr)
	}
	var exact []json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &exact); err != nil || len(exact) != 10_000 {
		t.Fatalf("10,000 record result has len %d, decode error %v", len(exact), err)
	}

	for _, testCase := range []string{"malformed", "duplicate", "unknown", "over", "partial"} {
		t.Run(testCase, func(t *testing.T) {
			stdout, stderr, err := runPresetScript(t, "huggingface-repositories-json.sh", home, bin,
				[]string{"HF_CASE=" + testCase})
			if err == nil {
				t.Fatalf("case %s unexpectedly succeeded: %s", testCase, stdout)
			}
			if stdout != "" {
				t.Fatalf("case %s emitted partial output: %q", testCase, stdout)
			}
			if !strings.Contains(stderr, "cannot prove a complete repository inventory") {
				t.Fatalf("case %s stderr = %q", testCase, stderr)
			}
		})
	}

	if _, err := os.Stat(filepath.Join(bin, "python")); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly supplied a Python interpreter: %v", err)
	}
}
