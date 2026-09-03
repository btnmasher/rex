package esi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestResolveAllianceUsesESIIdentityAndIconEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/alliances/99013797":
			_, _ = writer.Write([]byte(`{"name":"The Caldari Fourth District"}`))
		case "/alliances/99013797/icons":
			_, _ = writer.Write([]byte(`{"px64x64":"https://images.evetech.net/alliances/99013797/logo?size=64"}`))
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	entity, err := client.ResolveAlliance(context.Background(), "99013797")
	if err != nil {
		t.Fatalf("resolve alliance: %v", err)
	}
	if entity.Name != "The Caldari Fourth District" || entity.IconURL == "" {
		t.Fatalf("unexpected alliance entity: %#v", entity)
	}
}

func TestResolveCharacterAndCorporationUsePublicIdentityEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/characters/2119766668":
			_, _ = writer.Write([]byte(`{"name":"Attacking Pilot"}`))
		case "/corporations/98774989":
			_, _ = writer.Write([]byte(`{"name":"Fourth District Sentinels"}`))
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	character, err := client.ResolveCharacter(context.Background(), "2119766668")
	if err != nil {
		t.Fatalf("resolve character: %v", err)
	}
	corporation, err := client.ResolveCorporation(context.Background(), "98774989")
	if err != nil {
		t.Fatalf("resolve corporation: %v", err)
	}
	if character.Name != "Attacking Pilot" || corporation.Name != "Fourth District Sentinels" {
		t.Fatalf("unexpected identities: character=%#v corporation=%#v", character, corporation)
	}
}

func TestResolveRegionAndStructureTypeUsePublicUniverseEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/universe/regions/10000015":
			_, _ = writer.Write([]byte(`{"name":"Pure Blind"}`))
		case "/universe/types/35834":
			_, _ = writer.Write([]byte(`{"name":"Metenox Moon Drill"}`))
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	region, err := client.ResolveRegion(context.Background(), "10000015")
	if err != nil {
		t.Fatalf("resolve region: %v", err)
	}
	typeEntity, err := client.ResolveStructureType(context.Background(), "35834")
	if err != nil {
		t.Fatalf("resolve structure type: %v", err)
	}
	if region.Name != "Pure Blind" || typeEntity.Name != "Metenox Moon Drill" {
		t.Fatalf("unexpected entities: region=%#v type=%#v", region, typeEntity)
	}
}

func TestResolveCorporationSkipsConditionalETag(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/corporations/98590623" {
			t.Errorf("unexpected path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Header.Get("If-None-Match") != "" {
			t.Errorf("identity lookup unexpectedly used conditional ETag")
			return
		}
		requests.Add(1)
		writer.Header().Set("ETag", `"corporation-v1"`)
		_, _ = writer.Write([]byte(`{"name":"Previous Owner Corporation"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	for range 2 {
		entity, resolveErr := client.ResolveCorporation(context.Background(), "98590623")
		if resolveErr != nil {
			t.Fatalf("resolve corporation: %v", resolveErr)
		}
		if entity.Name != "Previous Owner Corporation" {
			t.Fatalf("corporation name = %q", entity.Name)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("HTTP requests = %d, want two unconditional identity requests", requests.Load())
	}
}

func TestResolveSolarSystemSkipsConditionalETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("If-None-Match") != "" {
			t.Errorf("solar-system lookup unexpectedly used conditional ETag for %s", request.URL.Path)
			return
		}
		switch request.URL.Path {
		case "/universe/systems/30000142":
			_, _ = writer.Write([]byte(`{"name":"Jita","constellation_id":20000020}`))
		case "/universe/constellations/20000020":
			_, _ = writer.Write([]byte(`{"region_id":10000002}`))
		case "/universe/regions/10000002":
			_, _ = writer.Write([]byte(`{"name":"The Forge"}`))
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	resolved, err := client.ResolveSolarSystem(context.Background(), "30000142")
	if err != nil {
		t.Fatalf("resolve solar system: %v", err)
	}
	if resolved.Name != "Jita" || resolved.RegionName != "The Forge" {
		t.Fatalf("unexpected solar system: %#v", resolved)
	}
}
