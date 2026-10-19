package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProductionRouterVersionAndLoopbackAuth(t *testing.T) {
	for _, tc := range []struct {
		name, host, auth, token string
		want                    int
	}{
		{"IP", "127.0.0.1:9723", "Bearer private", "private", 200},
		{"localhost", "localhost:9723", "Bearer private", "private", 200},
		{"bare-host", "localhost", "Bearer private", "private", 200},
		{"rebinding", "evil.test:9723", "Bearer private", "private", 403},
		{"wrong-port", "127.0.0.1:1234", "Bearer private", "private", 403},
		{"missing-token", "localhost:9723", "", "private", 401},
		{"wrong-token", "localhost:9723", "Bearer wrong", "private", 401},
		{"no-auth", "localhost:9723", "", "", 200},
		{"no-auth-rebinding", "evil.test", "", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRouter(New(nil, nil), NewEventHub(), "v-test", "c-test")
			r.Use(LoopbackAuth(9723, tc.token))
			req := httptest.NewRequest("GET", "/api/v1/version", nil)
			req.Host = tc.host
			req.Header.Set("Authorization", tc.auth)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusOK {
				var got map[string]string
				if json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != 2 || got["version"] != "v-test" || got["commit"] != "c-test" {
					t.Fatal("version contract changed")
				}
			}
		})
	}
}
