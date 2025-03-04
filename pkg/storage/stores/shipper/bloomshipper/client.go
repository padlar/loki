package bloomshipper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"io"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/concurrency"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"

	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/chunk/client"
	"github.com/grafana/loki/pkg/storage/config"
	"github.com/grafana/loki/pkg/storage/stores/shipper/indexshipper/tsdb"
	"github.com/grafana/loki/pkg/util/encoding"
)

const (
	rootFolder            = "bloom"
	metasFolder           = "metas"
	bloomsFolder          = "blooms"
	delimiter             = "/"
	fileNamePartDelimiter = "-"
)

type Ref struct {
	TenantID                     string
	TableName                    string
	Bounds                       v1.FingerprintBounds
	StartTimestamp, EndTimestamp model.Time
	Checksum                     uint32
}

// Hash hashes the ref
// NB(owen-d): we don't include the tenant in the hash
// as it's not included in the data and leaving it out gives
// flexibility for migrating data between tenants
func (r Ref) Hash(h hash.Hash32) error {
	if err := r.Bounds.Hash(h); err != nil {
		return err
	}

	var enc encoding.Encbuf

	enc.PutString(r.TableName)
	enc.PutBE64(uint64(r.StartTimestamp))
	enc.PutBE64(uint64(r.EndTimestamp))
	enc.PutBE32(r.Checksum)

	_, err := h.Write(enc.Get())
	return errors.Wrap(err, "writing BlockRef")
}

// Cmp returns the fingerprint's position relative to the bounds
func (r Ref) Cmp(fp uint64) v1.BoundsCheck {
	return r.Bounds.Cmp(model.Fingerprint(fp))
}

func (r Ref) Interval() Interval {
	return NewInterval(r.StartTimestamp, r.EndTimestamp)
}

type BlockRef struct {
	Ref
}

func (r BlockRef) String() string {
	return defaultKeyResolver{}.Block(r).Addr()
}

type MetaRef struct {
	Ref
}

func (r MetaRef) String() string {
	return defaultKeyResolver{}.Meta(r).Addr()
}

// todo rename it
type Meta struct {
	MetaRef `json:"-"`

	// The specific TSDB files used to generate the block.
	Sources []tsdb.SingleTenantTSDBIdentifier

	// TODO(owen-d): remove, unused
	// Old blocks which can be deleted in the future. These should be from previous compaction rounds.
	BlockTombstones []BlockRef

	// A list of blocks that were generated
	Blocks []BlockRef
}

func MetaRefFrom(
	tenant,
	table string,
	bounds v1.FingerprintBounds,
	sources []tsdb.SingleTenantTSDBIdentifier,
	blocks []BlockRef,
) (MetaRef, error) {

	h := v1.Crc32HashPool.Get()
	defer v1.Crc32HashPool.Put(h)

	err := bounds.Hash(h)
	if err != nil {
		return MetaRef{}, errors.Wrap(err, "writing OwnershipRange")
	}

	for _, source := range sources {
		err = source.Hash(h)
		if err != nil {
			return MetaRef{}, errors.Wrap(err, "writing Sources")
		}
	}

	var (
		start, end model.Time
	)

	for i, block := range blocks {
		if i == 0 || block.StartTimestamp.Before(start) {
			start = block.StartTimestamp
		}

		if block.EndTimestamp.After(end) {
			end = block.EndTimestamp
		}

		err = block.Hash(h)
		if err != nil {
			return MetaRef{}, errors.Wrap(err, "writing Blocks")
		}
	}

	return MetaRef{
		Ref: Ref{
			TenantID:       tenant,
			TableName:      table,
			Bounds:         bounds,
			StartTimestamp: start,
			EndTimestamp:   end,
			Checksum:       h.Sum32(),
		},
	}, nil

}

type MetaSearchParams struct {
	TenantID string
	Interval Interval
	Keyspace v1.FingerprintBounds
}

type MetaClient interface {
	KeyResolver
	GetMeta(ctx context.Context, ref MetaRef) (Meta, error)
	GetMetas(ctx context.Context, refs []MetaRef) ([]Meta, error)
	PutMeta(ctx context.Context, meta Meta) error
	DeleteMetas(ctx context.Context, refs []MetaRef) error
}

type Block struct {
	BlockRef
	Data io.ReadSeekCloser
}

// CloseableReadSeekerAdapter is a wrapper around io.ReadSeeker to make it io.Closer
// if it doesn't already implement it.
type ClosableReadSeekerAdapter struct {
	io.ReadSeeker
}

func (c ClosableReadSeekerAdapter) Close() error {
	if closer, ok := c.ReadSeeker.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func BlockFrom(tenant, table string, blk *v1.Block) (Block, error) {
	md, _ := blk.Metadata()
	ref := Ref{
		TenantID:       tenant,
		TableName:      table,
		Bounds:         md.Series.Bounds,
		StartTimestamp: md.Series.FromTs,
		EndTimestamp:   md.Series.ThroughTs,
		Checksum:       md.Checksum,
	}

	// TODO(owen-d): pool
	buf := bytes.NewBuffer(nil)
	err := v1.TarGz(buf, blk.Reader())

	if err != nil {
		return Block{}, errors.Wrap(err, "archiving+compressing block")
	}

	reader := bytes.NewReader(buf.Bytes())

	return Block{
		BlockRef: BlockRef{Ref: ref},
		Data:     ClosableReadSeekerAdapter{reader},
	}, nil
}

type BlockClient interface {
	KeyResolver
	GetBlock(ctx context.Context, ref BlockRef) (BlockDirectory, error)
	GetBlocks(ctx context.Context, refs []BlockRef) ([]BlockDirectory, error)
	PutBlock(ctx context.Context, block Block) error
	DeleteBlocks(ctx context.Context, refs []BlockRef) error
}

type Client interface {
	MetaClient
	BlockClient
	IsObjectNotFoundErr(err error) bool
	Stop()
}

// Compiler check to ensure BloomClient implements the Client interface
var _ Client = &BloomClient{}

type BloomClient struct {
	KeyResolver
	concurrency int
	client      client.ObjectClient
	logger      log.Logger
	fsResolver  KeyResolver
}

func NewBloomClient(cfg bloomStoreConfig, client client.ObjectClient, logger log.Logger) (*BloomClient, error) {
	return &BloomClient{
		KeyResolver: defaultKeyResolver{}, // TODO(owen-d): hook into schema, similar to `{,Parse}ExternalKey`
		fsResolver:  NewPrefixedResolver(cfg.workingDir, defaultKeyResolver{}),
		concurrency: cfg.numWorkers,
		client:      client,
		logger:      logger,
	}, nil
}

func (b *BloomClient) IsObjectNotFoundErr(err error) bool {
	return b.client.IsObjectNotFoundErr(err)
}

func (b *BloomClient) PutMeta(ctx context.Context, meta Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("can not marshal the meta to json: %w", err)
	}
	key := b.Meta(meta.MetaRef).Addr()
	return b.client.PutObject(ctx, key, bytes.NewReader(data))
}

func (b *BloomClient) DeleteMetas(ctx context.Context, refs []MetaRef) error {
	err := concurrency.ForEachJob(ctx, len(refs), b.concurrency, func(ctx context.Context, idx int) error {
		key := b.Meta(refs[idx]).Addr()
		return b.client.DeleteObject(ctx, key)
	})

	return err
}

// GetBlock downloads the blocks from objectStorage and returns the downloaded block
func (b *BloomClient) GetBlock(ctx context.Context, ref BlockRef) (BlockDirectory, error) {
	key := b.Block(ref).Addr()
	readCloser, _, err := b.client.GetObject(ctx, key)
	if err != nil {
		return BlockDirectory{}, fmt.Errorf("failed to get block from storage: %w", err)
	}

	path := b.fsResolver.Block(ref).LocalPath()
	err = extractBlock(readCloser, path, b.logger)
	if err != nil {
		return BlockDirectory{}, fmt.Errorf("failed to extract block into directory : %w", err)
	}

	return NewBlockDirectory(ref, path, b.logger), nil
}

func (b *BloomClient) GetBlocks(ctx context.Context, refs []BlockRef) ([]BlockDirectory, error) {
	// TODO(chaudum): Integrate download queue
	// The current implementation does brute-force download of all blocks with maximum concurrency.
	// However, we want that a single block is downloaded only exactly once, even if it is requested
	// multiple times concurrently.
	results := make([]BlockDirectory, len(refs))
	err := concurrency.ForEachJob(ctx, len(refs), b.concurrency, func(ctx context.Context, idx int) error {
		block, err := b.GetBlock(ctx, refs[idx])
		if err != nil {
			return err
		}
		results[idx] = block
		return nil
	})

	return results, err
}

func (b *BloomClient) PutBlock(ctx context.Context, block Block) error {
	defer func(Data io.ReadCloser) {
		_ = Data.Close()
	}(block.Data)

	key := b.Block(block.BlockRef).Addr()
	_, err := block.Data.Seek(0, 0)
	if err != nil {
		return fmt.Errorf("error uploading block file %s : %w", key, err)
	}

	err = b.client.PutObject(ctx, key, block.Data)
	if err != nil {
		return fmt.Errorf("error uploading block file: %w", err)
	}
	return nil
}

func (b *BloomClient) DeleteBlocks(ctx context.Context, references []BlockRef) error {
	return concurrency.ForEachJob(ctx, len(references), b.concurrency, func(ctx context.Context, idx int) error {
		ref := references[idx]
		key := b.Block(ref).Addr()
		err := b.client.DeleteObject(ctx, key)

		if err != nil {
			return fmt.Errorf("error deleting block file: %w", err)
		}
		return nil
	})
}

func (b *BloomClient) Stop() {
	b.client.Stop()
}

func (b *BloomClient) GetMetas(ctx context.Context, refs []MetaRef) ([]Meta, error) {
	results := make([]Meta, len(refs))
	err := concurrency.ForEachJob(ctx, len(refs), b.concurrency, func(ctx context.Context, idx int) error {
		meta, err := b.GetMeta(ctx, refs[idx])
		if err != nil {
			return err
		}
		results[idx] = meta
		return nil
	})
	return results, err
}

func (b *BloomClient) GetMeta(ctx context.Context, ref MetaRef) (Meta, error) {
	meta := Meta{
		MetaRef: ref,
	}
	key := b.KeyResolver.Meta(ref).Addr()
	reader, _, err := b.client.GetObject(ctx, key)
	if err != nil {
		return Meta{}, fmt.Errorf("error downloading meta file %s : %w", key, err)
	}
	defer reader.Close()

	err = json.NewDecoder(reader).Decode(&meta)
	if err != nil {
		return Meta{}, fmt.Errorf("error unmarshalling content of meta file %s: %w", key, err)
	}
	return meta, nil
}

func findPeriod(configs []config.PeriodConfig, ts model.Time) (config.DayTime, error) {
	for i := len(configs) - 1; i >= 0; i-- {
		periodConfig := configs[i]
		if !periodConfig.From.Time.After(ts) {
			return periodConfig.From, nil
		}
	}
	return config.DayTime{}, fmt.Errorf("can not find period for timestamp %d", ts)
}
