// Package publish uploads a built artifact to S3, in parallel parts. It is the
// only write path in Range: everything else treats a remote object as
// immutable, and there is deliberately no pull.
package publish
