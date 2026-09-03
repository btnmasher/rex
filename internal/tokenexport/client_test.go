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

	"github.com/google/uuid"
)

func TestNewClientRejectsMalformedBaseURLWithoutPanicking(t *testing.T) {
	if _, err := NewClient("%", "secret", nil); err == nil {
		t.Fatal("expected malformed base URL to fail")
	}
}

func TestClientListsCorporationsAndFetchesScopedExports(t *testing.T) {
	const bearer = "endpoint-secret"
	server := newExportServer(t, bearer)
	defer server.Close()

	client, err := NewClient(server.URL+"/auth", bearer, server.Client())
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
			writeJSON(t, writer, Response{
				Corporations: []CorporationTokenGroup{{
					CorporationID:   corporationID,
					CorporationName: "Corp",
					Tokens: []AccessTokenRecord{{
						CharacterID:   "900000001",
						CharacterName: "Pilot",
						UserID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
						Role:          "member",
						AccessToken:   "access",
						ExpiresAt:     time.Now().Add(time.Hour),
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
		tokens := make([]AccessTokenRecord, 0, maxTokensPerCorp+1)
		for i := range maxTokensPerCorp + 1 {
			tokens = append(tokens, AccessTokenRecord{
				CharacterID:   strconv.Itoa(i + 1),
				CharacterName: "Pilot " + strconv.Itoa(i+1),
				UserID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
				Role:          "member",
				AccessToken:   "access",
				ExpiresAt:     time.Now().Add(time.Hour),
			})
		}
		response := Response{Corporations: []CorporationTokenGroup{{CorporationID: "100", CorporationName: "Corp", Tokens: tokens}}}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "secret", server.Client())
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
	client, err := NewClient(server.URL, "super-secret", server.Client())
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
