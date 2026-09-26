// Package profile records and replays working sets. A Recorder notes which
// blocks a session demanded and when; Merge folds that into a Profile stored
// beside the cache, keyed by artifact identity, block size and workload name;
// the next session replays it ahead of demand. The file format is specified in
// docs/PROFILE_FORMAT.md.
package profile
