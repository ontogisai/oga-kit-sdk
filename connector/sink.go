package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Point is one time-series measurement a connector emits (Tier-C). It mirrors
// the platform's internal/timeseries.Point shape MINUS tenant_id: the serving
// tenant is stamped by the platform timeseries intake from the connector's
// authenticated credential, never from the payload.
type Point struct {
	// SourceID identifies the physical source (sensor/meter) the reading
	// came from. Mapped to an entity by the platform's per-tenant config.
	SourceID string `json:"source_id"`

	// EntityID optionally binds the reading directly to a KG entity. When
	// empty the platform resolves it from SourceID via the tenant mapping.
	EntityID string `json:"entity_id,omitempty"`

	// Metric is the measurement name (e.g. "temperature", "discharge_pressure").
	Metric string `json:"metric"`

	// Unit is the measurement unit (e.g. "C", "kPa").
	Unit string `json:"unit,omitempty"`

	// Value is the measurement value.
	Value float64 `json:"value"`

	// Quality is an optional source quality flag (0 = good).
	Quality int `json:"quality,omitempty"`

	// Timestamp is when the measurement was taken. Zero defaults to receipt
	// time at the intake. (No omitempty: time.Time is a struct, so the tag
	// would be a no-op; the intake interprets the zero value.)
	Timestamp time.Time `json:"timestamp"`
}

// TimeseriesSink is the Tier-C emit surface: a connector pushes standardized
// points to the platform's tenant-safe timeseries intake. The connector never
// writes TimeSeriesPoint directly; the sink targets the platform intake, which
// stamps tenant and maps source→entity before writing.
type TimeseriesSink interface {
	// EmitPoints submits a batch of measurements. Implementations should be
	// safe for sequential calls from a single binding's goroutine.
	EmitPoints(ctx context.Context, points []Point) error
}

// TokenProvider supplies the current bearer token (the sidecar workload token)
// for authenticating to the platform intake. It is called per request so token
// rotation is transparent.
type TokenProvider func(ctx context.Context) (string, error)

// IntakePath is the platform timeseries-intake path the HTTP sink posts to.
// The platform side (tenant-safe intake) implements the receiving end and
// stamps tenant from the bearer credential. Body shape: {"points":[Point,...]}.
const IntakePath = "/ingest/timeseries"

// httpTimeseriesSink is the default TimeseriesSink: it POSTs a JSON batch to
// the platform timeseries intake with the workload bearer token.
type httpTimeseriesSink struct {
	baseURL string
	token   TokenProvider
	client  *http.Client
}

// NewHTTPTimeseriesSink builds a TimeseriesSink that posts batches to
// baseURL+IntakePath, authenticating with the token from tp. baseURL is the
// platform ingress base (e.g. the gateway URL); tp supplies the sidecar
// workload token.
func NewHTTPTimeseriesSink(baseURL string, tp TokenProvider) TimeseriesSink {
	return &httpTimeseriesSink{
		baseURL: baseURL,
		token:   tp,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type timeseriesBatch struct {
	Points []Point `json:"points"`
}

func (s *httpTimeseriesSink) EmitPoints(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	body, err := json.Marshal(timeseriesBatch{Points: points})
	if err != nil {
		return fmt.Errorf("timeseries sink: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+IntakePath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("timeseries sink: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != nil {
		tok, terr := s.token(ctx)
		if terr != nil {
			return fmt.Errorf("timeseries sink: token: %w", terr)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("timeseries sink: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("timeseries sink: intake HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// errSink is the default sink when none is configured: it fails loudly so a
// connector that emits timeseries without wiring a sink is caught immediately
// rather than silently dropping points.
type errSink struct{}

func (errSink) EmitPoints(_ context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	return errors.New("connector: no TimeseriesSink configured (set Config.Sink)")
}
