package router

import (
	"context"
	"errors"
	"fmt"
	"github.com/xynogen/ogc/internal/client"
	"github.com/xynogen/ogc/internal/config"
	"testing"
	"time"
)

func TestRequestErrorsDoNotBlockOtherSessions(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := NewFallbackHandler(nil, 3, time.Minute)
			models := []config.ModelConfig{{ModelID: "test"}}
			calls := 0
			for i := 0; i < 5; i++ {
				_, _, err := h.ExecuteWithFallback(context.Background(), models, func(context.Context, config.ModelConfig) ([]byte, error) {
					calls++
					return nil, fmt.Errorf("wrapped: %w", &client.APIError{StatusCode: status, Body: "test"})
				})
				var api *client.APIError
				if !errors.As(err, &api) || api.StatusCode != status {
					t.Fatal("lost upstream status", err)
				}
			}
			if calls != 5 || h.GetCircuitStates()["test"] != "closed" {
				t.Fatal("request error blocked model")
			}
			_, _, err := h.ExecuteWithFallback(context.Background(), models, func(context.Context, config.ModelConfig) ([]byte, error) { return []byte("OK"), nil })
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCancellationDoesNotOpenCircuit(t *testing.T) {
	h := NewFallbackHandler(nil, 3, time.Minute)
	for i := 0; i < 5; i++ {
		_, _, err := h.ExecuteWithFallback(context.Background(), []config.ModelConfig{{ModelID: "test"}}, func(context.Context, config.ModelConfig) ([]byte, error) {
			return nil, fmt.Errorf("wrapped: %w", context.Canceled)
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if h.GetCircuitStates()["test"] != "closed" {
		t.Fatal("cancellation opened breaker")
	}
}
func TestServerOutageStillOpensCircuit(t *testing.T) {
	h := NewFallbackHandler(nil, 3, time.Minute)
	calls := 0
	for i := 0; i < 4; i++ {
		h.ExecuteWithFallback(context.Background(), []config.ModelConfig{{ModelID: "test"}}, func(context.Context, config.ModelConfig) ([]byte, error) {
			calls++
			return nil, &client.APIError{StatusCode: 503}
		})
	}
	if calls != 3 || h.GetCircuitStates()["test"] != "open" {
		t.Fatal("outage breaker broken")
	}
}
func TestNeutralProbeReleasesCapacity(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Nanosecond)
	cb.RecordFailure()
	time.Sleep(time.Millisecond)
	for i := 0; i < 8; i++ {
		if !cb.AllowRequest() {
			t.Fatal("neutral probes exhausted capacity")
		}
		cb.RecordNeutral()
	}
}
