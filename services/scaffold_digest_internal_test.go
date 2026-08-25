package services

import (
	"testing"
	"testing/fstest"
)

func TestDigestTreeFramesPathsAndContents(t *testing.T) {
	left, err := digestTree(fstest.MapFS{
		"SKILL.md": &fstest.MapFile{Data: []byte("instructions")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	right, err := digestTree(fstest.MapFS{
		"SKILL.m": &fstest.MapFile{Data: []byte("dinstructions")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("different skill trees share a digest because path and content were not framed")
	}
}
