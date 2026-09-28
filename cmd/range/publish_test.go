package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/rangetest"
)

func TestPublishRejectsNonS3Target(t *testing.T) {
	rangetest.QuietStdout(t)
	file := filepath.Join(t.TempDir(), "dev.img")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commandPublish([]string{file, "/tmp/elsewhere"}); err == nil {
		t.Error("publish accepted a target that is not object storage")
	}
	if err := commandPublish([]string{file}); err == nil {
		t.Error("publish accepted a single argument")
	}
	if err := commandPublish([]string{"--part-size", "1MiB", file, "s3://b/k"}); err == nil {
		t.Error("publish accepted a part size below the S3 minimum")
	}
}
