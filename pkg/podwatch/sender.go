package podwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gfhw/inferguard/pkg/log"
)

type EventSender struct {
	endpoint string
	method   string
	headers  map[string]string
	client   *http.Client
}

func NewEventSender(endpoint string) *EventSender {
	return &EventSender{
		endpoint: endpoint,
		method:   http.MethodPost,
		headers: map[string]string{
			"Content-Type": "application/json",
		},
		client: &http.Client{},
	}
}

func NewEventSenderWithConfig(endpoint string, method string, headers map[string]string) *EventSender {
	if method == "" {
		method = http.MethodPost
	}
	method = strings.ToUpper(method)

	h := map[string]string{
		"Content-Type": "application/json",
	}
	for k, v := range headers {
		h[k] = v
	}

	return &EventSender{
		endpoint: endpoint,
		method:   method,
		headers:  h,
		client:   &http.Client{},
	}
}

func (s *EventSender) Send(ctx context.Context, event PodEvent) error {
	if s.endpoint == "" {
		return nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, s.method, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	for k, v := range s.headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Warn("Event push returned non-2xx status",
			"status", resp.StatusCode,
			"endpoint", s.endpoint,
			"method", s.method)
		return fmt.Errorf("event push failed with status %d", resp.StatusCode)
	}

	return nil
}

// SendRaw posts a raw JSON string directly — used for user-defined alert bodies.
func (s *EventSender) SendRaw(ctx context.Context, rawJSON string) error {
	if s.endpoint == "" || rawJSON == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, s.method, s.endpoint, strings.NewReader(rawJSON))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send raw alert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("raw alert push failed with status %d", resp.StatusCode)
	}
	return nil
}

func (s *EventSender) UpdateConfig(endpoint string, method string, headers map[string]string) {
	if endpoint != "" {
		s.endpoint = endpoint
	}
	if method != "" {
		s.method = strings.ToUpper(method)
	}
	if headers != nil {
		h := map[string]string{
			"Content-Type": "application/json",
		}
		for k, v := range headers {
			h[k] = v
		}
		s.headers = h
	}
}