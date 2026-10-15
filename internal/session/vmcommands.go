package session

import (
	"context"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/vm"
)

// VMCommands serves, on the host, the commands a session in Range's VM runs
// from other images: what finder says, and the images, opened with config.
func VMCommands(finder Finder, config core.Config) vm.Commands {
	return vmCommands{finder, config}
}

type vmCommands struct {
	Finder
	config core.Config
}

func (c vmCommands) Open(ctx context.Context, image string) (vm.ImageSource, error) {
	config := c.config
	config.Quiet = true
	return core.Open(ctx, image, config)
}
