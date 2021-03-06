// Package s3 implements Storage based on an S3 bucket.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/efarrer/iothrottler"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/retrying"
)

const (
	s3storageType = "s3"
)

type s3Storage struct {
	Options

	ctx context.Context

	cli *minio.Client

	downloadThrottler *iothrottler.IOThrottlerPool
	uploadThrottler   *iothrottler.IOThrottlerPool
}

func (s *s3Storage) GetBlob(ctx context.Context, b blob.ID, offset, length int64) ([]byte, error) {
	attempt := func() ([]byte, error) {
		var opt minio.GetObjectOptions

		if length > 0 {
			if err := opt.SetRange(offset, offset+length-1); err != nil {
				return nil, errors.Wrap(blob.ErrInvalidRange, "unable to set range")
			}
		}

		o, err := s.cli.GetObject(ctx, s.BucketName, s.getObjectNameString(b), opt)
		if err != nil {
			return nil, errors.Wrap(err, "GetObject")
		}

		defer o.Close() //nolint:errcheck

		throttled, err := s.downloadThrottler.AddReader(o)
		if err != nil {
			return nil, errors.Wrap(err, "AddReader")
		}

		v, err := ioutil.ReadAll(throttled)
		if err != nil {
			return nil, errors.Wrap(err, "ReadAll")
		}

		if length == 0 {
			return []byte{}, nil
		}

		return v, nil
	}

	fetched, err := attempt()
	if err != nil {
		return nil, translateError(err)
	}

	return blob.EnsureLengthExactly(fetched, length)
}

func translateError(err error) error {
	var me minio.ErrorResponse

	if errors.As(err, &me) {
		switch me.StatusCode {
		case http.StatusOK:
			return nil

		case http.StatusNotFound:
			return blob.ErrBlobNotFound

		case http.StatusRequestedRangeNotSatisfiable:
			return blob.ErrInvalidRange
		}
	}

	return err
}

func (s *s3Storage) GetMetadata(ctx context.Context, b blob.ID) (blob.Metadata, error) {
	oi, err := s.cli.StatObject(ctx, s.BucketName, s.getObjectNameString(b), minio.StatObjectOptions{})
	if err != nil {
		return blob.Metadata{}, errors.Wrap(translateError(err), "StatObject")
	}

	return blob.Metadata{
		BlobID:    b,
		Length:    oi.Size,
		Timestamp: oi.LastModified,
	}, nil
}

func (s *s3Storage) PutBlob(ctx context.Context, b blob.ID, data blob.Bytes) error {
	throttled, err := s.uploadThrottler.AddReader(ioutil.NopCloser(data.Reader()))
	if err != nil {
		return errors.Wrap(err, "AddReader")
	}

	combinedLength := data.Length()

	progressCallback := blob.ProgressCallback(ctx)
	if progressCallback != nil {
		progressCallback(string(b), 0, int64(combinedLength))
		defer progressCallback(string(b), int64(combinedLength), int64(combinedLength))
	}

	uploadInfo, err := s.cli.PutObject(ctx, s.BucketName, s.getObjectNameString(b), throttled, int64(combinedLength), minio.PutObjectOptions{
		ContentType: "application/x-kopia",
		Progress:    newProgressReader(progressCallback, string(b), int64(combinedLength)),
	})

	if errors.Is(err, io.EOF) && uploadInfo.Size == 0 {
		// special case empty stream
		_, err = s.cli.PutObject(ctx, s.BucketName, s.getObjectNameString(b), bytes.NewBuffer(nil), 0, minio.PutObjectOptions{
			ContentType: "application/x-kopia",
		})
	}

	// nolint:wrapcheck
	return err
}

func (s *s3Storage) SetTime(ctx context.Context, b blob.ID, t time.Time) error {
	return blob.ErrSetTimeUnsupported
}

func (s *s3Storage) DeleteBlob(ctx context.Context, b blob.ID) error {
	err := translateError(s.cli.RemoveObject(ctx, s.BucketName, s.getObjectNameString(b), minio.RemoveObjectOptions{}))
	if errors.Is(err, blob.ErrBlobNotFound) {
		return nil
	}

	return translateError(err)
}

func (s *s3Storage) getObjectNameString(b blob.ID) string {
	return s.Prefix + string(b)
}

func (s *s3Storage) ListBlobs(ctx context.Context, prefix blob.ID, callback func(blob.Metadata) error) error {
	oi := s.cli.ListObjects(ctx, s.BucketName, minio.ListObjectsOptions{
		Prefix: s.getObjectNameString(prefix),
	})
	for o := range oi {
		if err := o.Err; err != nil {
			return err
		}

		bm := blob.Metadata{
			BlobID:    blob.ID(o.Key[len(s.Prefix):]),
			Length:    o.Size,
			Timestamp: o.LastModified,
		}

		if err := callback(bm); err != nil {
			return err
		}
	}

	return nil
}

func (s *s3Storage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{
		Type:   s3storageType,
		Config: &s.Options,
	}
}

func (s *s3Storage) Close(ctx context.Context) error {
	return nil
}

func (s *s3Storage) String() string {
	return fmt.Sprintf("s3://%v/%v", s.BucketName, s.Prefix)
}

func (s *s3Storage) DisplayName() string {
	return fmt.Sprintf("S3: %v %v", s.Endpoint, s.BucketName)
}

type progressReader struct {
	cb           blob.ProgressFunc
	blobID       string
	completed    int64
	totalLength  int64
	lastReported int64
}

func (r *progressReader) Read(b []byte) (int, error) {
	r.completed += int64(len(b))
	if r.completed >= r.lastReported+1000000 && r.completed < r.totalLength {
		r.cb(r.blobID, r.completed, r.totalLength)
		r.lastReported = r.completed
	}

	return len(b), nil
}

func newProgressReader(cb blob.ProgressFunc, blobID string, totalLength int64) io.Reader {
	if cb == nil {
		return nil
	}

	return &progressReader{cb: cb, blobID: blobID, totalLength: totalLength}
}

func toBandwidth(bytesPerSecond int) iothrottler.Bandwidth {
	if bytesPerSecond <= 0 {
		return iothrottler.Unlimited
	}

	return iothrottler.Bandwidth(bytesPerSecond) * iothrottler.BytesPerSecond
}

func getCustomTransport(insecureSkipVerify bool) (transport *http.Transport) {
	// nolint:gosec
	customTransport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}}
	return customTransport
}

// New creates new S3-backed storage with specified options:
//
// - the 'BucketName' field is required and all other parameters are optional.
func New(ctx context.Context, opt *Options) (blob.Storage, error) {
	if opt.BucketName == "" {
		return nil, errors.New("bucket name must be specified")
	}

	minioOpts := &minio.Options{
		Creds:  credentials.NewStaticV4(opt.AccessKeyID, opt.SecretAccessKey, opt.SessionToken),
		Secure: !opt.DoNotUseTLS,
		Region: opt.Region,
	}

	if opt.DoNotVerifyTLS {
		minioOpts.Transport = getCustomTransport(true)
	}

	cli, err := minio.New(opt.Endpoint, minioOpts)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create client")
	}

	downloadThrottler := iothrottler.NewIOThrottlerPool(toBandwidth(opt.MaxDownloadSpeedBytesPerSecond))
	uploadThrottler := iothrottler.NewIOThrottlerPool(toBandwidth(opt.MaxUploadSpeedBytesPerSecond))

	ok, err := cli.BucketExists(ctx, opt.BucketName)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to determine if bucket %q exists", opt.BucketName)
	}

	if !ok {
		return nil, errors.Errorf("bucket %q does not exist", opt.BucketName)
	}

	return retrying.NewWrapper(&s3Storage{
		Options:           *opt,
		ctx:               ctx,
		cli:               cli,
		downloadThrottler: downloadThrottler,
		uploadThrottler:   uploadThrottler,
	}), nil
}

func init() {
	blob.AddSupportedStorage(
		s3storageType,
		func() interface{} {
			return &Options{}
		},
		func(ctx context.Context, o interface{}) (blob.Storage, error) {
			return New(ctx, o.(*Options))
		})
}
