package metastore

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/thanos-io/objstore"

	"github.com/grafana/loki/v3/pkg/dataobj"
	"github.com/grafana/loki/v3/pkg/dataobj/consumer/logsobj"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/streams"
	"github.com/grafana/loki/v3/pkg/logproto"
)

const (
	labelNameStart = "__start__"
	labelNameEnd   = "__end__"
	labelNamePath  = "__path__"
)

// Define our own builder config because metastore objects are significantly smaller.
var metastoreBuilderCfg = logsobj.BuilderConfig{
	TargetObjectSize:  32 * 1024 * 1024,
	TargetPageSize:    4 * 1024 * 1024,
	BufferSize:        32 * 1024 * 1024, // 8x page size
	TargetSectionSize: 4 * 1024 * 1024,  // object size / 8

	SectionStripeMergeLimit: 2,
}

type Updater struct {
	metastoreBuilder *logsobj.Builder
	tenantID         string
	metrics          *metastoreMetrics
	bucket           objstore.Bucket
	logger           log.Logger
	backoff          *backoff.Backoff
	buf              *bytes.Buffer

	builderOnce sync.Once
}

func NewUpdater(bucket objstore.Bucket, tenantID string, logger log.Logger) *Updater {
	metrics := newMetastoreMetrics()

	return &Updater{
		bucket:   bucket,
		metrics:  metrics,
		logger:   logger,
		tenantID: tenantID,
		backoff: backoff.New(context.TODO(), backoff.Config{
			MinBackoff: 50 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
		}),
		builderOnce: sync.Once{},
	}
}

func (m *Updater) RegisterMetrics(reg prometheus.Registerer) error {
	return m.metrics.register(reg)
}

func (m *Updater) UnregisterMetrics(reg prometheus.Registerer) {
	m.metrics.unregister(reg)
}

func (m *Updater) initBuilder() error {
	var initErr error
	m.builderOnce.Do(func() {
		metastoreBuilder, err := logsobj.NewBuilder(metastoreBuilderCfg)
		if err != nil {
			initErr = err
			return
		}
		m.buf = bytes.NewBuffer(make([]byte, 0, metastoreBuilderCfg.TargetObjectSize))
		m.metastoreBuilder = metastoreBuilder
	})
	return initErr
}

// Update adds provided dataobj path to the metastore. Flush stats are used to determine the stored metadata about this dataobj.
func (m *Updater) Update(ctx context.Context, dataobjPath string, minTimestamp, maxTimestamp time.Time) error {
	var err error
	processingTime := prometheus.NewTimer(m.metrics.metastoreProcessingTime)
	defer processingTime.ObserveDuration()

	// Initialize builder if this is the first call for this partition
	if err := m.initBuilder(); err != nil {
		return err
	}

	// Work our way through the metastore objects window by window, updating & creating them as needed.
	// Each one handles its own retries in order to keep making progress in the event of a failure.
	for metastorePath := range iterStorePaths(m.tenantID, minTimestamp, maxTimestamp) {
		m.backoff.Reset()
		for m.backoff.Ongoing() {
			err = m.bucket.GetAndReplace(ctx, metastorePath, func(existing io.Reader) (io.Reader, error) {
				m.buf.Reset()
				if existing != nil {
					level.Debug(m.logger).Log("msg", "found existing metastore, updating", "path", metastorePath)
					_, err := io.Copy(m.buf, existing)
					if err != nil {
						return nil, errors.Wrap(err, "copying to local buffer")
					}
				} else {
					level.Debug(m.logger).Log("msg", "no existing metastore found, creating new one", "path", metastorePath)
				}

				m.metastoreBuilder.Reset()

				if m.buf.Len() > 0 {
					replayDuration := prometheus.NewTimer(m.metrics.metastoreReplayTime)
					object, err := dataobj.FromReaderAt(bytes.NewReader(m.buf.Bytes()), int64(m.buf.Len()))
					if err != nil {
						return nil, errors.Wrap(err, "creating object from buffer")
					}
					if err := m.readFromExisting(ctx, object); err != nil {
						return nil, errors.Wrap(err, "reading existing metastore version")
					}
					replayDuration.ObserveDuration()
				}

				encodingDuration := prometheus.NewTimer(m.metrics.metastoreEncodingTime)

				ls := labels.New(
					labels.Label{Name: labelNameStart, Value: strconv.FormatInt(minTimestamp.UnixNano(), 10)},
					labels.Label{Name: labelNameEnd, Value: strconv.FormatInt(maxTimestamp.UnixNano(), 10)},
					labels.Label{Name: labelNamePath, Value: dataobjPath},
				)
				err := m.metastoreBuilder.Append(logproto.Stream{
					Labels:  ls.String(),
					Entries: []logproto.Entry{{Line: ""}},
				})
				if err != nil {
					return nil, errors.Wrap(err, "appending internal metadata stream")
				}

				m.buf.Reset()
				_, err = m.metastoreBuilder.Flush(m.buf)
				if err != nil {
					return nil, errors.Wrap(err, "flushing metastore builder")
				}
				encodingDuration.ObserveDuration()
				return m.buf, nil
			})
			if err == nil {
				level.Info(m.logger).Log("msg", "successfully merged & updated metastore", "metastore", metastorePath)
				m.metrics.incMetastoreWrites(statusSuccess)
				break
			}
			level.Error(m.logger).Log("msg", "failed to get and replace metastore object", "err", err, "metastore", metastorePath)
			m.metrics.incMetastoreWrites(statusFailure)
			m.backoff.Wait()
		}
		// Reset at the end too so we don't leave our memory hanging around between calls.
		m.metastoreBuilder.Reset()
	}
	return err
}

// readFromExisting reads the provided metastore object and appends the streams to the builder so it can be later modified.
func (m *Updater) readFromExisting(ctx context.Context, object *dataobj.Object) error {
	var streamsReader streams.RowReader
	defer streamsReader.Close()

	// Read streams from existing metastore object and write them to the builder for the new object
	buf := make([]streams.Stream, 100)

	for _, section := range object.Sections() {
		if !streams.CheckSection(section) {
			continue
		}
		sec, err := streams.Open(ctx, section)
		if err != nil {
			return errors.Wrap(err, "opening section")
		}

		streamsReader.Reset(sec)
		for n, err := streamsReader.Read(ctx, buf); n > 0; n, err = streamsReader.Read(ctx, buf) {
			if err != nil && err != io.EOF {
				return errors.Wrap(err, "reading streams")
			}
			for _, stream := range buf[:n] {
				err = m.metastoreBuilder.Append(logproto.Stream{
					Labels:  stream.Labels.String(),
					Entries: []logproto.Entry{{Line: ""}},
				})
				if err != nil {
					return errors.Wrap(err, "appending streams")
				}
			}
		}
	}

	return nil
}
