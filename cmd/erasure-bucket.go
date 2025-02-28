// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"context"
	"errors"

	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/minio/internal/sync/errgroup"
)

// list all errors that can be ignore in a bucket operation.
var bucketOpIgnoredErrs = append(baseIgnoredErrs, errDiskAccessDenied, errUnformattedDisk)

// list all errors that can be ignored in a bucket metadata operation.
var bucketMetadataOpIgnoredErrs = append(bucketOpIgnoredErrs, errVolumeNotFound)

// Bucket operations

// MakeBucket - make a bucket.
func (er erasureObjects) MakeBucketWithLocation(ctx context.Context, bucket string, opts BucketOptions) error {
	defer NSUpdated(bucket, slashSeparator)

	// Verify if bucket is valid.
	if err := s3utils.CheckValidBucketNameStrict(bucket); err != nil {
		return BucketNameInvalid{Bucket: bucket}
	}

	storageDisks := er.getDisks()

	g := errgroup.WithNErrs(len(storageDisks))

	// Make a volume entry on all underlying storage disks.
	for index := range storageDisks {
		index := index
		g.Go(func() error {
			if storageDisks[index] != nil {
				if err := storageDisks[index].MakeVol(ctx, bucket); err != nil {
					if !errors.Is(err, errVolumeExists) {
						logger.LogIf(ctx, err)
					}
					return err
				}
				return nil
			}
			return errDiskNotFound
		}, index)
	}

	writeQuorum := getWriteQuorum(len(storageDisks))
	err := reduceWriteQuorumErrs(ctx, g.Wait(), bucketOpIgnoredErrs, writeQuorum)
	return toObjectErr(err, bucket)
}

func undoDeleteBucket(storageDisks []StorageAPI, bucket string) {
	g := errgroup.WithNErrs(len(storageDisks))
	// Undo previous make bucket entry on all underlying storage disks.
	for index := range storageDisks {
		if storageDisks[index] == nil {
			continue
		}
		index := index
		g.Go(func() error {
			_ = storageDisks[index].MakeVol(context.Background(), bucket)
			return nil
		}, index)
	}

	// Wait for all make vol to finish.
	g.Wait()
}

// getBucketInfo - returns the BucketInfo from one of the load balanced disks.
func (er erasureObjects) getBucketInfo(ctx context.Context, bucketName string) (bucketInfo BucketInfo, err error) {
	storageDisks := er.getDisks()

	g := errgroup.WithNErrs(len(storageDisks))
	var bucketsInfo = make([]BucketInfo, len(storageDisks))
	// Undo previous make bucket entry on all underlying storage disks.
	for index := range storageDisks {
		index := index
		g.Go(func() error {
			if storageDisks[index] == nil {
				return errDiskNotFound
			}
			volInfo, err := storageDisks[index].StatVol(ctx, bucketName)
			if err != nil {
				return err
			}
			bucketsInfo[index] = BucketInfo(volInfo)
			return nil
		}, index)
	}

	errs := g.Wait()

	for i, err := range errs {
		if err == nil {
			return bucketsInfo[i], nil
		}
	}

	// If all our errors were ignored, then we try to
	// reduce to one error based on read quorum.
	// `nil` is deliberately passed for ignoredErrs
	// because these errors were already ignored.
	readQuorum := getReadQuorum(len(storageDisks))
	return BucketInfo{}, reduceReadQuorumErrs(ctx, errs, nil, readQuorum)
}

// GetBucketInfo - returns BucketInfo for a bucket.
func (er erasureObjects) GetBucketInfo(ctx context.Context, bucket string) (bi BucketInfo, e error) {
	bucketInfo, err := er.getBucketInfo(ctx, bucket)
	if err != nil {
		return bi, toObjectErr(err, bucket)
	}
	return bucketInfo, nil
}

// DeleteBucket - deletes a bucket.
func (er erasureObjects) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	// Collect if all disks report volume not found.
	defer NSUpdated(bucket, slashSeparator)

	storageDisks := er.getDisks()

	g := errgroup.WithNErrs(len(storageDisks))

	for index := range storageDisks {
		index := index
		g.Go(func() error {
			if storageDisks[index] != nil {
				return storageDisks[index].DeleteVol(ctx, bucket, opts.Force)
			}
			return errDiskNotFound
		}, index)
	}

	// Wait for all the delete vols to finish.
	dErrs := g.Wait()

	if opts.Force {
		for _, err := range dErrs {
			if err != nil {
				undoDeleteBucket(storageDisks, bucket)
				return toObjectErr(err, bucket)
			}
		}

		return nil
	}

	writeQuorum := getWriteQuorum(len(storageDisks))
	err := reduceWriteQuorumErrs(ctx, dErrs, bucketOpIgnoredErrs, writeQuorum)
	if err == errErasureWriteQuorum && !opts.NoRecreate {
		undoDeleteBucket(storageDisks, bucket)
	}

	if err == nil || errors.Is(err, errVolumeNotFound) {
		var purgedDangling bool
		// At this point we have `err == nil` but some errors might be `errVolumeNotEmpty`
		// we should proceed to attempt a force delete of such buckets.
		for index, err := range dErrs {
			if err == errVolumeNotEmpty && storageDisks[index] != nil {
				storageDisks[index].RenameFile(ctx, bucket, "", minioMetaTmpDeletedBucket, mustGetUUID())
				purgedDangling = true
			}
		}
		// if we purged dangling buckets, ignore errVolumeNotFound error.
		if purgedDangling {
			err = nil
		}

	}

	return toObjectErr(err, bucket)
}

// IsNotificationSupported returns whether bucket notification is applicable for this layer.
func (er erasureObjects) IsNotificationSupported() bool {
	return true
}

// IsListenSupported returns whether listen bucket notification is applicable for this layer.
func (er erasureObjects) IsListenSupported() bool {
	return true
}

// IsEncryptionSupported returns whether server side encryption is implemented for this layer.
func (er erasureObjects) IsEncryptionSupported() bool {
	return true
}

// IsCompressionSupported returns whether compression is applicable for this layer.
func (er erasureObjects) IsCompressionSupported() bool {
	return true
}

// IsTaggingSupported indicates whether erasureObjects implements tagging support.
func (er erasureObjects) IsTaggingSupported() bool {
	return true
}
