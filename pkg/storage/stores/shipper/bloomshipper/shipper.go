package bloomshipper

import (
	"context"
	"fmt"
	"sort"

	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
)

type ForEachBlockCallback func(bq *v1.BlockQuerier, bounds v1.FingerprintBounds) error

type Interface interface {
	ForEach(ctx context.Context, tenant string, blocks []BlockRef, callback ForEachBlockCallback) error
	Stop()
}

type Shipper struct {
	store Store
}

type Limits interface {
	BloomGatewayBlocksDownloadingParallelism(tenantID string) int
}

func NewShipper(client Store) *Shipper {
	return &Shipper{store: client}
}

// ForEach is a convenience function that wraps the store's FetchBlocks function
// and automatically closes the block querier once the callback was run.
func (s *Shipper) ForEach(ctx context.Context, refs []BlockRef, callback ForEachBlockCallback) error {
	bqs, err := s.store.FetchBlocks(ctx, refs)
	if err != nil {
		return err
	}

	if len(bqs) != len(refs) {
		return fmt.Errorf("number of response (%d) does not match number of requests (%d)", len(bqs), len(refs))
	}

	for i := range bqs {
		err := callback(bqs[i].BlockQuerier, bqs[i].Bounds)
		// close querier to decrement ref count
		bqs[i].Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Shipper) Stop() {
	s.store.Stop()
}

// BlocksForMetas returns all the blocks from all the metas listed that are within the requested bounds
// and not tombstoned in any of the metas
func BlocksForMetas(metas []Meta, interval Interval, keyspaces []v1.FingerprintBounds) []BlockRef {
	blocks := make(map[BlockRef]bool) // block -> isTombstoned

	for _, meta := range metas {
		for _, tombstone := range meta.BlockTombstones {
			blocks[tombstone] = true
		}
		for _, block := range meta.Blocks {
			tombstoned, ok := blocks[block]
			if ok && tombstoned {
				// skip tombstoned blocks
				continue
			}
			blocks[block] = false
		}
	}

	refs := make([]BlockRef, 0, len(blocks))
	for ref, tombstoned := range blocks {
		if !tombstoned && !isOutsideRange(ref, interval, keyspaces) {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Bounds.Less(refs[j].Bounds)
	})

	return refs
}

// isOutsideRange tests if a given BlockRef b is outside of search boundaries
// defined by min/max timestamp and min/max fingerprint.
// Fingerprint ranges must be sorted in ascending order.
func isOutsideRange(b BlockRef, interval Interval, bounds []v1.FingerprintBounds) bool {
	// check time interval
	if !interval.Overlaps(b.Interval()) {
		return true
	}

	// check fingerprint ranges
	for _, keyspace := range bounds {
		if keyspace.Overlaps(b.Bounds) {
			return false
		}
	}

	return true
}
