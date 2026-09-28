package publish

import (
	"context"
	"errors"
	"testing"
)

func TestBoundedGroupKeepsFirstError(t *testing.T) {
	group, ctx := newBoundedGroup(context.Background(), 2)
	group.start(func() error { return errors.New("first") })
	group.start(func() error {
		<-ctx.Done() // cancelled by the failure above
		return nil
	})
	if err := group.wait(); err == nil {
		t.Fatal("group swallowed the error")
	}
}
