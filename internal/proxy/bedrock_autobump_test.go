package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

type mockServiceQuotas struct {
	mu       sync.Mutex
	current  float64
	requests []float64
	codes    []string
}

func (m *mockServiceQuotas) GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(m.current)}}, nil
}

func (m *mockServiceQuotas) RequestServiceQuotaIncrease(ctx context.Context, in *servicequotas.RequestServiceQuotaIncreaseInput, _ ...func(*servicequotas.Options)) (*servicequotas.RequestServiceQuotaIncreaseOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, *in.DesiredValue)
	m.codes = append(m.codes, *in.QuotaCode)
	return &servicequotas.RequestServiceQuotaIncreaseOutput{RequestedQuota: &sqtypes.RequestedServiceQuotaChange{Id: aws.String("req-1")}}, nil
}

func newTestBumper(mock serviceQuotasAPI) *bedrockQuotaBumper {
	return &bedrockQuotaBumper{client: mock, cooldown: time.Hour, maxValue: 20_000_000, last: map[string]time.Time{}}
}

func TestBumperDoublesAndDedupes(t *testing.T) {
	mock := &mockServiceQuotas{current: 200_000}
	b := newTestBumper(mock)

	b.onThrottle("us.anthropic.claude-fable-5")
	if len(mock.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(mock.requests))
	}
	if mock.requests[0] != 400_000 {
		t.Fatalf("desired = %v, want 400000 (2x)", mock.requests[0])
	}
	if mock.codes[0] != "L-9B258944" {
		t.Fatalf("quota code = %q, want fable TPM code", mock.codes[0])
	}

	// Second throttle within cooldown must not re-request.
	b.onThrottle("us.anthropic.claude-fable-5")
	if len(mock.requests) != 1 {
		t.Fatalf("cooldown breached: %d requests", len(mock.requests))
	}
}

func TestBumperCapsAtMax(t *testing.T) {
	mock := &mockServiceQuotas{current: 15_000_000}
	b := newTestBumper(mock)
	b.onThrottle("opus")
	if len(mock.requests) != 1 || mock.requests[0] != 20_000_000 {
		t.Fatalf("expected capped request 20000000, got %v", mock.requests)
	}
}

func TestBumperIgnoresUnknownModel(t *testing.T) {
	mock := &mockServiceQuotas{current: 200_000}
	b := newTestBumper(mock)
	b.onThrottle("us.anthropic.claude-sonnet-5")
	if len(mock.requests) != 0 {
		t.Fatalf("unknown model should not request; got %v", mock.requests)
	}
}
