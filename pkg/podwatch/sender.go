package podwatch

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type EventSender struct {
	mu       sync.Mutex
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
		client: &http.Client{Timeout: 30 * time.Second},
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
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// config returns a consistent snapshot of the sender configuration under lock,
// so concurrent Send/SendRaw calls never observe a partially-updated endpoint,
// method, or header map.
func (s *EventSender) config() (endpoint, method string, headers map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := make(map[string]string, len(s.headers))
	for k, v := range s.headers {
		h[k] = v
	}
	return s.endpoint, s.method, h
}

// SendRaw posts a raw JSON string directly — used for user-defined alert bodies.
func (s *EventSender) SendRaw(ctx context.Context, rawJSON string) error {
	endpoint, method, headers := s.config()
	if endpoint == "" || rawJSON == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(rawJSON))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	for k, v := range headers {
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
	s.mu.Lock()
	defer s.mu.Unlock()
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