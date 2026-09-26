// Command range puts you inside a remote environment before it has been
// downloaded. An environment is an ordinary object in S3, on an HTTP server or
// on disk; range reads only the byte ranges a workload touches, caches them,
// and learns which ones the next session will want.
//
//	range build --from-oci golang:1.23 -o go.range
//	range publish go.range s3://bucket/go.range
//	range shell s3://bucket/go.range
//
// Run range with no arguments for the full command list, or see docs/CLI.md.
package main
