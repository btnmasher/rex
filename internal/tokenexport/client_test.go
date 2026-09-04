package tokenexport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewClientRejectsMalformedBaseURLWithoutPanicking(t *testing.T) {
	if _, err := NewClient(ClientConfig{BaseURL: "%", BearerToken: "secret", TokenCount: 60}); err == nil {
		t.Fatal("expected malformed base URL to fail")
	}
}

func TestNewClientRejectsInvalidTokenCount(t *testing.T) {
	for _, count := range []int{0, MaxTokensPerCorporation + 1} {
		if _, err := NewClient(ClientConfig{BaseURL: "http://localhost:3000", BearerToken: "secret", TokenCount: count}); err == nil {
			t.Fatalf("count %d unexpectedly succeeded", count)
		}
	}
}

func TestClientListsCorporationsAndFetchesScopedExports(t *testing.T) {
	const bearer = "endpoint-secret"
	server := newExportServer(t, bearer)
	defer server.Close()

	client, err := NewClient(ClientConfig{
		BaseURL:     server.URL + "/auth",
		BearerToken: bearer,
		TokenCount:  60,
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	corporations, err := client.ListCorporations(context.Background())
	if err != nil || len(corporations) != 2 || corporations[0] != "100" || corporations[1] != "200" {
		t.Fatalf("corporations = %+v, err = %v", corporations, err)
	}
	scoped, err := client.FetchCorporation(context.Background(), "200")
	if err != nil || len(scoped.Corporations) != 1 || scoped.Corporations[0].CorporationID != "200" {
		t.Fatalf("scoped response = %+v, err = %v", scoped, err)
	}
}

func newExportServer(t *testing.T, bearer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+bearer {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/auth" + corporationsPath:
			writeJSON(t, writer, CorporationListResponse{CorporationIDs: []string{"100", "200"}})
		case "/auth" + exportPath:
			corporationID := request.URL.Query().Get("corporationId")
			if corporationID == "" {
				corporationID = "100"
			}
			if request.URL.Query().Get("count") != "60" {
				t.Errorf("count = %q, want 60", request.URL.Query().Get("count"))
			}
			writeJSON(t, writer, Response{
				Corporations: []CorporationTokenGroup{{
					CorporationID:   corporationID,
					CorporationName: "Corp",
					Tokens: []AccessTokenRecord{{
						CharacterID: "900000001",
						AccessToken: "access",
						ExpiresAt:   time.Now().Add(time.Hour),
					}},
				}},
			})
		default:
			t.Errorf("path = %q", request.URL.Path)
		}
	}))
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestClientRejectsInvalidAndOversizedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		tokens := make([]AccessTokenRecord, 0, MaxTokensPerCorporation+1)
		for i := range MaxTokensPerCorporation + 1 {
			tokens = append(tokens, AccessTokenRecord{
				CharacterID: strconv.Itoa(i + 1),
				AccessToken: "access",
				ExpiresAt:   time.Now().Add(time.Hour),
			})
		}
		response := Response{Corporations: []CorporationTokenGroup{{CorporationID: "100", CorporationName: "Corp", Tokens: tokens}}}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL:     server.URL,
		BearerToken: "secret",
		TokenCount:  60,
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchCorporation(context.Background(), "not-an-id"); err == nil {
		t.Fatal("expected invalid corporation ID to fail")
	}
	if _, err := client.FetchCorporation(context.Background(), "100"); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("expected invalid response, got %v", err)
	}
}

func TestClientClassifiesUnauthorizedWithoutLeakingBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL:     server.URL,
		BearerToken: "super-secret",
		TokenCount:  60,
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListCorporations(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
	if err.Error() == "" || contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked bearer: %v", err)
	}
}

func contains(value, needle string) bool {
	return strings.Contains(value, needle)
}
