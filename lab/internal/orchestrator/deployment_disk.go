package orchestrator

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/pfap/lab/internal/model"
)

// deploymentDiskPreflight checks every target before Deploy changes any worker.
// Runtime storage is reserved even when a cached runtime might be reusable:
// retaining both the uploaded archive and extracted files is the worst case.
// Cold miners can generate independent temporary DAGs concurrently, even with
// a shared DAG directory. Reserve current/next datasets for each planned miner.
func (o Orchestrator) deploymentDiskPreflight(ctx context.Context, exp model.Experiment, servers map[string]model.Server, artifact string) error {
	nodes, err := model.ResolvePlannedMiners(exp, servers)
	if err != nil {
		return err
	}
	var targets []string
	seen := make(map[string]bool)
	for _, placement := range exp.Placements {
		if _, ok := servers[placement.ServerID]; !ok {
			return fmt.Errorf("server %s not found", placement.ServerID)
		}
		if !seen[placement.ServerID] {
			targets = append(targets, placement.ServerID)
			seen[placement.ServerID] = true
		}
	}
	miningServers := make(map[string]uint64)
	for _, node := range nodes {
		if node.IsMiner {
			miningServers[node.ServerID]++
		}
	}
	runtimeBytes, err := runtimeArchiveDiskBytes(ctx, artifact)
	if err != nil {
		return err
	}
	for _, id := range targets {
		required := MinimumDiskFreeBytes
		if miningServers[id] > 0 {
			required = miningDiskRequiredBytesForMiners(0, miningServers[id])
		}
		if runtimeBytes > math.MaxUint64-required {
			return fmt.Errorf("runtime archive disk reservation overflows capacity")
		}
		if err := o.CheckDisk(ctx, servers[id], required+runtimeBytes); err != nil {
			return fmt.Errorf("deployment disk preflight (includes runtime archive and extracted files): %w", err)
		}
	}
	return nil
}

func runtimeArchiveDiskBytes(ctx context.Context, artifact string) (uint64, error) {
	file, err := os.Open(artifact)
	if err != nil {
		return 0, fmt.Errorf("inspect runtime archive disk requirements: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspect runtime archive disk requirements: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, fmt.Errorf("runtime archive must be a regular file")
	}
	compressed, err := gzip.NewReader(diskArchiveContextReader{ctx, file})
	if err != nil {
		return 0, fmt.Errorf("inspect runtime archive disk requirements: %w", err)
	}
	defer compressed.Close()
	total := uint64(info.Size())
	archive := tar.NewReader(compressed)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("inspect runtime archive disk requirements: %w", err)
		}
		if header.Size < 0 || uint64(header.Size) > math.MaxUint64-total {
			return 0, fmt.Errorf("runtime archive disk reservation overflows capacity")
		}
		total += uint64(header.Size)
	}
	// tar EOF can precede the gzip footer. Read it to detect corrupt/truncated
	// archives before any remote directories, uploads or node processes exist.
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return 0, fmt.Errorf("inspect runtime archive disk requirements: %w", err)
	}
	return total, nil
}

type diskArchiveContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r diskArchiveContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(data)
}
