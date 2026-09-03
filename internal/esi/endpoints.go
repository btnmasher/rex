package esi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const (
	characterPath              = "/characters/%s"
	characterNotificationsPath = "/characters/%s/notifications"
	corporationPath            = "/corporations/%s"
	alliancePath               = "/alliances/%s"
	allianceIconsPath          = "/alliances/%s/icons"
	solarSystemPath            = "/universe/systems/%s"
	constellationPath          = "/universe/constellations/%s"
	regionPath                 = "/universe/regions/%s"
	structureTypePath          = "/universe/types/%s"
	planetPath                 = "/universe/planets/%s"
	moonPath                   = "/universe/moons/%s"
)

// Endpoint identifies an ESI resource and its cache identity.
type Endpoint struct {
	Method   string
	Path     string
	RouteKey string
	CacheKey string
}

// Validate ensures an endpoint is safe to resolve against the configured ESI host.
func (e Endpoint) Validate() error {
	if strings.TrimSpace(e.Method) == "" {
		return errors.New("ESI endpoint method is required")
	}
	if strings.TrimSpace(e.Path) == "" || !strings.HasPrefix(e.Path, "/") {
		return errors.New("ESI endpoint path must be absolute and relative to the host")
	}
	parsed, err := url.Parse(e.Path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
		return errors.New("ESI endpoint path must be a relative URL")
	}
	return nil
}

func characterNotificationsEndpoint(characterID string) (Endpoint, error) {
	if strings.TrimSpace(characterID) == "" {
		return Endpoint{}, errors.New("ESI character ID is required")
	}
	path := formatPath(characterNotificationsPath, url.PathEscape(strings.TrimSpace(characterID)))
	return Endpoint{
		Method:   http.MethodGet,
		Path:     path,
		RouteKey: "/characters/:id/notifications",
		CacheKey: path,
	}, nil
}

func getEntityEndpoint(template, routeKey, id string) (Endpoint, error) {
	if strings.TrimSpace(id) == "" {
		return Endpoint{}, errors.New("ESI entity ID is required")
	}
	path := formatPath(template, url.PathEscape(strings.TrimSpace(id)))
	return Endpoint{
		Method:   http.MethodGet,
		Path:     path,
		RouteKey: routeKey,
		CacheKey: path,
	}, nil
}

func formatPath(template, value string) string {
	return strings.Replace(template, "%s", value, 1)
}

func normalizeRouteKey(path string) string {
	parsed, err := url.Parse(path)
	if err != nil {
		return path
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index, segment := range segments {
		if isNumeric(segment) {
			segments[index] = ":id"
		}
	}
	if len(segments) == 0 || segments[0] == "" {
		return "/"
	}
	return "/" + strings.Join(segments, "/")
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
