package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/core"
)

func commandStats(args []string) error {
	c, err := core.ReadConfig()
	if err != nil {
		return err
	}
	var tracePath string
	fs := flagSet("stats", &c, &tracePath)
	if err := fs.Parse(args); err != nil {
		return err
	}
	filter := ""
	if rest := fs.Args(); len(rest) > 0 {
		filter = rest[0]
	}
	found, err := loadAllStats(c.CacheDir)
	if err != nil {
		return err
	}
	shown := 0
	for _, s := range found {
		if filter != "" && s.URI != filter {
			continue
		}
		printStats(s)
		shown++
	}
	if shown == 0 {
		fmt.Println("No artifact statistics yet. Open an artifact first.")
	}
	return nil
}

func loadAllStats(cacheDir string) ([]core.Stats, error) {
	root := filepath.Join(cacheDir, "objects")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []core.Stats
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "stats.json"))
		if err != nil {
			continue
		}
		var s core.Stats
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		found = append(found, s)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].UpdatedAt.After(found[j].UpdatedAt) })
	return found, nil
}

func printStats(s core.Stats) {
	if s.SessionID != "" {
		fmt.Printf("Session\n")
		fmt.Printf("  ID                  %s\n", s.SessionID)
		if s.SessionName != "" {
			fmt.Printf("  Name                %s\n", s.SessionName)
		}
		if s.Workload != "" {
			fmt.Printf("  Workload            %s\n", s.Workload)
		}
		fmt.Printf("  Ready               %s\n", roundedSeconds(time.Duration(s.ReadyNanos)))
		fmt.Println()
	}
	fmt.Printf("Artifact\n")
	fmt.Printf("  URI                 %s\n", s.URI)
	fmt.Printf("  Logical size        %s\n", bytesize.Format(s.Size))
	if s.StoredSize > 0 {
		fmt.Printf("  Stored size         %s  (%.1fx)\n", bytesize.Format(s.StoredSize),
			float64(s.Size)/float64(s.StoredSize))
	}
	fmt.Printf("  Block size          %s\n", bytesize.Format(s.BlockSize))
	fmt.Printf("\nThis session\n")
	fmt.Printf("  Working set         %s\n", bytesize.Format(s.WorkingSetBytes))
	fmt.Printf("  Working-set ratio   %.4f%%\n", s.WorkingSetRatio())
	fmt.Printf("  Remote transferred  %s\n", bytesize.Format(s.SessionRemoteBytes))
	fmt.Printf("  Transfer ratio      %.4f%%\n", s.TransferRatio())
	fmt.Printf("  Requests            %d\n", s.SessionRequests)
	fmt.Printf("  Fetch amplification %.2fx\n", s.Amplification())
	fmt.Printf("\nLifetime for this artifact\n")
	fmt.Printf("  Logical reads       %d\n", s.LogicalReads)
	fmt.Printf("  Logical bytes       %s\n", bytesize.Format(s.RequestedBytes))
	fmt.Printf("  Remote transferred  %s over %d requests\n", bytesize.Format(s.RemoteBytes), s.RemoteRequests)
	fmt.Printf("  Demand bytes        %s\n", bytesize.Format(s.DemandBytes))
	fmt.Printf("  Prefetch bytes      %s\n", bytesize.Format(s.PrefetchBytes))
	fmt.Printf("  Average latency     %s\n", s.AverageRemoteLatency().Round(100*time.Microsecond))
	fmt.Printf("  Retries             %d\n", s.Retries)
	fmt.Printf("\nCache\n")
	fmt.Printf("  Memory hits         %d\n", s.MemoryHits)
	fmt.Printf("  Disk hits           %d\n", s.DiskHits)
	fmt.Printf("  Misses              %d\n", s.Misses)
	fmt.Printf("  Coalesced waits     %d\n", s.Coalesced)
	fmt.Printf("  Overall hit rate    %.1f%%\n", s.HitRate())
	fmt.Printf("  Prefetched          %d (%d used)\n", s.PrefetchIssued, s.PrefetchHits)
	fmt.Printf("  Cached locally      %s\n\n", bytesize.Format(s.CachedBytes))
}
