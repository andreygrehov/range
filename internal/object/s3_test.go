package object

import (
	"testing"
)

func TestSplitS3URI(t *testing.T) {
	tests := []struct {
		uri            string
		bucket, key    string
		expectsFailure bool
	}{
		{"s3://demo/linux.img", "demo", "linux.img", false},
		{"s3://demo/a/b/c.img", "demo", "a/b/c.img", false},
		{"s3://demo", "", "", true},
		{"s3://demo/", "", "", true},
		{"s3:///key", "", "", true},
	}
	for _, tc := range tests {
		bucket, key, err := SplitS3URI(tc.uri)
		if tc.expectsFailure {
			if err == nil {
				t.Errorf("splitS3URI(%q) should have failed", tc.uri)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitS3URI(%q): %v", tc.uri, err)
			continue
		}
		if bucket != tc.bucket || key != tc.key {
			t.Errorf("splitS3URI(%q) = (%q,%q), want (%q,%q)", tc.uri, bucket, key, tc.bucket, tc.key)
		}
	}
}
