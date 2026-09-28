package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

// key identifies artifact + block size + workload. Nothing
// machine-specific enters it, so a profile file is portable between hosts.
func key(ident object.Identity, workload string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		ident.URI, strconv.FormatInt(ident.Size, 10), ident.ETag, ident.VersionID,
		strconv.FormatInt(ident.BlockSize, 10), workload,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Path is where the profile for an artifact and workload is stored.
func Path(cacheDir string, ident object.Identity, workload string) string {
	return filepath.Join(cacheDir, "profiles", key(ident, workload)+".json")
}

var errVersion = errors.New("range: unsupported profile version")

// ReadFile reads a profile, refusing versions it does not understand.
func ReadFile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	return Parse(data)
}

// Parse reads a profile from its JSON, refusing versions it does not understand.
func Parse(data []byte) (Profile, error) {
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return Profile{}, err
	}
	if p.Version != version {
		return Profile{}, fmt.Errorf("%w: %d", errVersion, p.Version)
	}
	return p, nil
}

// Load returns the stored profile for an artifact and workload, if there is
// one that matches its identity and block size.
func Load(cacheDir string, ident object.Identity, blockSize int64, workload string) (Profile, bool) {
	p, err := ReadFile(Path(cacheDir, ident, workload))
	if err != nil || !p.matches(ident, blockSize, workload) {
		return Profile{}, false
	}
	return p, len(p.Ranges) > 0
}

// WriteFile writes to a temporary file and renames, so a crash
// can never leave a half-written profile behind.
func WriteFile(path string, p Profile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".profile-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// withLock serialises concurrent sessions updating the same profile.
//
// The lock is an advisory flock on a file that is never unlinked. The kernel
// drops it when the holder exits, so a crashed writer cannot wedge the profile
// and there is no stale-lock timeout to guess at. Unlinking the file while
// another process holds a descriptor on it is precisely the race that makes
// break-and-retry schemes unsafe, so it stays.
func withLock(path string, fn func() error) error {
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("profile %s is locked by another session", filepath.Base(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

// merge folds one session's observations into whatever was already
// learned. Blocks are unioned; counts, timings and the session total grow.
func merge(existing Profile, ident object.Identity, blockSize int64, workload string,
	observed map[int64]time.Duration, now time.Time) Profile {

	stats := map[int64]*blockStat{}
	sessions := int64(0)
	if existing.matches(ident, blockSize, workload) {
		stats = existing.expand()
		sessions = existing.Sessions
	}
	for block, at := range observed {
		ms := at.Milliseconds()
		stat, ok := stats[block]
		if !ok {
			stat = &blockStat{firstSeenMs: ms}
			stats[block] = stat
		}
		stat.observations++
		stat.firstUseSum += ms
		if ms < stat.firstSeenMs {
			stat.firstSeenMs = ms
		}
		stat.lastSeen = now
	}
	sessions++

	// Without decay a profile only ever unions, so a block touched once a year
	// ago is prefetched forever and the working set drifts away from what the
	// workload actually does now.
	cutoff := now.Add(-maxAge)
	for block, stat := range stats {
		if !stat.lastSeen.IsZero() && stat.lastSeen.Before(cutoff) {
			delete(stats, block)
		}
	}

	blocks := make([]int64, 0, len(stats))
	for block := range stats {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })

	// Group adjacent blocks whose histories agree. Blocks read together in one
	// burst share a bucket and collapse into a run; blocks with different
	// histories stay separate so no per-block information is invented.
	var ranges []BlockRange
	for _, block := range blocks {
		stat := stats[block]
		candidate := BlockRange{
			StartBlock: block, Count: 1, Observations: stat.observations,
			FirstSeenMillis: bucketMillis(stat.firstSeenMs),
			MeanFirstUseMs:  bucketMillis(stat.firstUseSum / stat.observations),
			LastSeen:        stat.lastSeen.Format(time.RFC3339),
		}
		if n := len(ranges); n > 0 {
			last := &ranges[n-1]
			if last.StartBlock+last.Count == block &&
				last.Observations == candidate.Observations &&
				last.FirstSeenMillis == candidate.FirstSeenMillis &&
				last.MeanFirstUseMs == candidate.MeanFirstUseMs &&
				last.LastSeen == candidate.LastSeen {
				last.Count++
				continue
			}
		}
		ranges = append(ranges, candidate)
	}
	return Profile{
		Version: version,
		Artifact: artifact{
			URI: ident.URI, Size: ident.Size, ETag: ident.ETag, VersionID: ident.VersionID,
		},
		BlockSize: blockSize,
		Workload:  workload,
		Sessions:  sessions,
		UpdatedAt: now.Format(time.RFC3339),
		Ranges:    ranges,
	}
}

// Save merges this session into the stored profile under a lock.
func Save(cacheDir string, ident object.Identity, blockSize int64, workload string,
	observed map[int64]time.Duration, now time.Time) (Profile, error) {

	path := Path(cacheDir, ident, workload)
	var merged Profile
	err := withLock(path, func() error {
		existing, _ := ReadFile(path)
		merged = merge(existing, ident, blockSize, workload, observed, now)
		return WriteFile(path, merged)
	})
	return merged, err
}
