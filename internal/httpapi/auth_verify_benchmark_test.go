package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

func BenchmarkHandleAuthVerify_HasUsers(b *testing.B) {
	dbURL := testDSN()
	if dbURL == "" {
		b.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL not set")
	}

	st, err := store.Open(dbURL)
	if err != nil {
		b.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	// Ensure DB actually has users so we don't hit the n==0 block.
	st.CreateUser(context.Background(), "bench@example.com", "password")
	defer st.DeleteUser(context.Background(), "bench@example.com")

	cfg := &config.Config{}
	srv := New(cfg, nil, st)

	reqBody := []byte(`{"email":"bench@example.com","password":"wrongpassword"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/verify", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()
		srv.handleAuthVerify(w, req)
	}
}

func BenchmarkHandleAuthVerify_NoUsers(b *testing.B) {
	dbURL := testDSN()
	if dbURL == "" {
		b.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL not set")
	}

	st, err := store.Open(dbURL)
	if err != nil {
		b.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	// Ensure DB actually has NO users to hit the n==0 block.
	users, _ := st.ListUsers(context.Background())
	for _, u := range users {
		st.DeleteUser(context.Background(), u.Email)
	}

	cfg := &config.Config{}
	srv := New(cfg, nil, st)

	reqBody := []byte(`{"email":"bench@example.com","password":"wrongpassword"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/verify", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()
		srv.handleAuthVerify(w, req)
	}
}
