package nbd

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/core/coretest"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestNBDAttachRefusesNonLinux(t *testing.T) {
	// The kernel path only exists on Linux; elsewhere it must say so clearly
	// rather than failing somewhere confusing inside an ioctl. On Linux as root
	// the attach succeeds and serves until disconnected, so there is nothing to
	// check and the test would block.
	if runtime.GOOS == "linux" {
		t.Skip("the refusal only happens off Linux")
	}
	data := rangetest.Data(8192)
	r, _ := coretest.NewReader(t, data, func(c *core.Config) { c.BlockSize = 4 << 10 })
	err := Attach(context.Background(), "/dev/nbd0", r, r.Size())
	if err == nil {
		t.Fatal("nbdAttach succeeded off Linux")
	}
	if !strings.Contains(err.Error(), "Linux") && !strings.Contains(err.Error(), "nbd") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
