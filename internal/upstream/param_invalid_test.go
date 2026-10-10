package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendChatV2SurfacesEmbeddedParamInvalidAsBadRequest(t *testing.T) {
	for _, payload := range []string{
		`{"error_code":"InferHub.001001005.400","error_msg":"invalid arguments"}`,
		`{"error_code":"400","error_msg":"The request param is invalid, Please check it"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+payload+"\n\n")
			}))
			defer backend.Close()
			c := NewWithChatEndpoint(time.Second, backend.URL)
			_, err := c.SendChatV2(context.Background(), map[string]any{"model": "GLM-5.2"}, "", SignCredential{}, "", false)
			var apiErr *ApiError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || !strings.Contains(apiErr.Message, "invalid") {
				t.Fatalf("parameter error was treated as an upstream failure: %T %v", err, err)
			}
		})
	}
}
