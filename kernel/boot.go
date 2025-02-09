package kernel

import (
	"fmt"
	"time"

	"github.com/MixinNetwork/mixin/kernel/internal/clock"
)

func (node *Node) Loop() error {
	go func() {
		err := node.ListenNeighbors()
		if err != nil {
			panic(fmt.Errorf("ListenNeighbors %s", err.Error()))
		}
	}()
	go func() {
		err := node.CosiLoop()
		if err != nil {
			panic(fmt.Errorf("CosiLoop %s", err.Error()))
		}
	}()
	go node.LoadCacheToQueue()
	go node.MintLoop()
	go node.ElectionLoop()
	return node.ConsumeQueue()
}

func TestMockDiff(at time.Duration) {
	clock.MockDiff(at)
}
