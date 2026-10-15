package client

import (
	"context"
	"fmt"

	xylemv1 "github.com/delefrati/xylem/gen/xylem/v1"
	"github.com/delefrati/xylem/internal/hash"
)

// packTarget packs uploaded blobs up to ~1 MiB per PutObjects message.
const packTarget = 1 << 20

// UploadStats counts what an upload sent.
type UploadStats struct {
	Blobs int64
	Bytes int64 // stored (compressed) bytes
}

// upload sends the cached blobs the server lacks (06 §Commit): HasObjects in
// batches of ≤ 1024, then one PutObjects stream packed up to ~1 MiB per
// message (a larger blob goes alone).
func (f *Fetcher) upload(ctx context.Context, ids []hash.ID) (UploadStats, error) {
	var stats UploadStats
	ids = dedupe(ids)
	var missing []hash.ID
	for start := 0; start < len(ids); start += fetchBatch {
		batch := ids[start:min(start+fetchBatch, len(ids))]
		raw := make([][]byte, len(batch))
		for i := range batch {
			raw[i] = batch[i][:]
		}
		resp, err := f.conn.Object.HasObjects(ctx, &xylemv1.HasObjectsRequest{Repo: f.repo, Ids: raw})
		if err != nil {
			return stats, err
		}
		for i, ok := range resp.GetPresent() {
			if !ok {
				missing = append(missing, batch[i])
			}
		}
	}
	if len(missing) == 0 {
		return stats, nil
	}
	stream, err := f.conn.Object.PutObjects(ctx)
	if err != nil {
		return stats, err
	}
	msg := &xylemv1.PutObjectsRequest{Repo: f.repo}
	size := 0
	send := func() error {
		if len(msg.Objects) == 0 {
			return nil
		}
		err := stream.Send(msg)
		msg, size = &xylemv1.PutObjectsRequest{Repo: f.repo}, 0
		return err
	}
	for _, id := range missing {
		data, err := f.cache.Get(id)
		if err != nil {
			return stats, fmt.Errorf("blob %s to upload: %w", id.Short(12), err)
		}
		if size > 0 && (size+len(data) > packTarget || len(msg.Objects) == fetchBatch) {
			if err := send(); err != nil {
				break // CloseAndRecv reports the server's error
			}
		}
		msg.Objects = append(msg.Objects, &xylemv1.StoredObject{Id: id[:], Data: data})
		size += len(data)
		stats.Blobs++
		stats.Bytes += int64(len(data))
	}
	if err := send(); err != nil {
		_, rerr := stream.CloseAndRecv()
		if rerr != nil {
			return stats, rerr
		}
		return stats, err
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return stats, err
	}
	return stats, nil
}

func dedupe(ids []hash.ID) []hash.ID {
	seen := make(map[hash.ID]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
