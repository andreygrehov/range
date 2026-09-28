// Package object reads byte ranges from the places artifacts live: S3, HTTP
// servers and local files. A Backend fetches one range and reports an object's
// identity; nothing here knows what the bytes mean.
//
// Reads are pinned to the generation of the object observed when it was
// opened - by S3 version ID, or by If-Match on the ETag - so an object that
// changes underneath a session fails with ErrChanged instead of mixing two
// generations of bytes.
package object
