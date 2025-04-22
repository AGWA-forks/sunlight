package ctlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type S3Backend struct {
	client        *s3.Client
	bucket        string
	keyPrefix     string
	metrics       []prometheus.Collector
	uploadSize    prometheus.Summary
	hedgeRequests prometheus.Counter
	hedgeWins     prometheus.Counter
	log           *slog.Logger
}

func NewS3Backend(ctx context.Context, region, bucket, endpoint, keyPrefix string, l *slog.Logger) (*S3Backend, error) {
	duration := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       "s3_request_duration_seconds",
			Help:       "S3 HTTP request latencies, by method and response code.",
			Objectives: map[float64]float64{0.5: 0.05, 0.75: 0.025, 0.9: 0.01, 0.99: 0.001},
			MaxAge:     1 * time.Minute,
			AgeBuckets: 6,
		},
		[]string{"method", "code"},
	)
	errors := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3_errors_total",
			Help: "S3 attempt error codes.",
		},
		[]string{"retryable", "errorcode"},
	)
	uploadSize := prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name:       "s3_upload_size_bytes",
			Help:       "S3 body size in bytes for object puts.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			MaxAge:     1 * time.Minute,
			AgeBuckets: 6,
		},
	)
	hedgeRequests := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "s3_hedges_total",
			Help: "S3 hedge requests that were launched because the main request was too slow.",
		},
	)
	hedgeWins := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "s3_hedges_successful_total",
			Help: "S3 hedge requests that completed before the main request.",
		},
	)

	transport := http.RoundTripper(http.DefaultTransport.(*http.Transport).Clone())
	transport = promhttp.InstrumentRoundTripperDuration(duration, transport)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for S3 backend: %w", err)
	}

	return &S3Backend{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.Region = region
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
			o.HTTPClient = &http.Client{Transport: transport}
			o.Retryer = retry.AddWithMaxBackoffDelay(retry.NewStandard(), 250*time.Millisecond)
			// The AttemptResults, arguably the right way to do this, are
			// exposed as part of middleware.Metadata, which is discarded by
			// PutObject on error.
			o.Retryer = &trackingRetryerV2{RetryerV2: o.Retryer.(aws.RetryerV2), errors: errors}
			o.Logger = awsLogger{log: l}
			o.ClientLogMode = aws.LogRequest | aws.LogResponse | aws.LogRetries
		}),
		bucket:        bucket,
		keyPrefix:     keyPrefix,
		metrics:       []prometheus.Collector{duration, uploadSize, hedgeRequests, hedgeWins},
		uploadSize:    uploadSize,
		hedgeRequests: hedgeRequests,
		hedgeWins:     hedgeWins,
		log:           l,
	}, nil
}

type trackingRetryerV2 struct {
	aws.RetryerV2
	errors *prometheus.CounterVec
}

func (r *trackingRetryerV2) IsErrorRetryable(err error) bool {
	code := "unknown"
	var e interface{ ErrorCode() string }
	if errors.As(err, &e) {
		code = e.ErrorCode()
	}

	v := r.RetryerV2.IsErrorRetryable(err)
	r.errors.WithLabelValues(fmt.Sprint(v), code).Inc()
	return v
}

type awsLogger struct {
	log *slog.Logger
}

func (l awsLogger) Logf(classification logging.Classification, format string, v ...interface{}) {
	if l.log.Enabled(context.Background(), slog.LevelDebug) {
		l.log.Debug("AWS SDK log entry", "classification", classification, "text", fmt.Sprintf(format, v...))
	}
}

var _ Backend = &S3Backend{}

func (s *S3Backend) Upload(ctx context.Context, key string, data []byte, opts *UploadOptions) error {
	start := time.Now()
	contentType := aws.String("application/octet-stream")
	if opts != nil && opts.ContentType != "" {
		contentType = aws.String(opts.ContentType)
	}
	var contentEncoding *string
	if opts != nil && opts.Compressed {
		contentEncoding = aws.String("gzip")
	}
	var cacheControl *string
	if opts != nil && opts.Immutable {
		cacheControl = aws.String("public, max-age=604800, immutable")
	}
	putObject := func() (*s3.PutObjectOutput, error) {
		return s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:          aws.String(s.bucket),
			Key:             aws.String(s.keyPrefix + key),
			Body:            bytes.NewReader(data),
			ContentLength:   aws.Int64(int64(len(data))),
			ContentEncoding: contentEncoding,
			ContentType:     contentType,
			CacheControl:    cacheControl,
		})
	}
	ctx, cancel := context.WithCancelCause(ctx)
	hedgeErr := make(chan error, 1)
	go func() {
		timer := time.NewTimer(75 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			s.hedgeRequests.Inc()
			_, err := putObject()
			s.log.DebugContext(ctx, "S3 PUT hedge", "key", key, "err", err)
			hedgeErr <- err
			cancel(errors.New("competing request succeeded"))
		}
	}()
	_, err := putObject()
	select {
	case err = <-hedgeErr:
		s.hedgeWins.Inc()
	default:
		cancel(errors.New("competing request succeeded"))
	}
	s.log.DebugContext(ctx, "S3 PUT", "key", key, "size", len(data),
		"compressed", contentEncoding != nil, "type", *contentType,
		"immutable", cacheControl != nil,
		"elapsed", time.Since(start), "err", err)
	s.uploadSize.Observe(float64(len(data)))
	if err != nil {
		return fmtErrorf("failed to upload %q to S3: %w", key, err)
	}
	return nil
}

func (s *S3Backend) Fetch(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyPrefix + key),
	})
	if err != nil {
		s.log.DebugContext(ctx, "S3 GET", "key", key, "err", err)
		return nil, fmtErrorf("failed to fetch %q from S3: %w", key, err)
	}
	defer out.Body.Close()
	s.log.DebugContext(ctx, "S3 GET", "key", key,
		"size", out.ContentLength, "encoding", out.ContentEncoding)
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmtErrorf("failed to read %q from S3: %w", key, err)
	}
	return data, nil
}

func (s *S3Backend) Metrics() []prometheus.Collector {
	return s.metrics
}
