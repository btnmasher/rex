// Package tokenexport accesses auth-next's corporation access-token export endpoint.
package tokenexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	exportPath        = "/api/internal/member-refresh-tokens"
	corporationsPath  = exportPath + "/corporations"
	maxResponseBytes  = 2 << 20
	maxTokensPerCorp  = 15
	maxRequestedCorps = 100
	maxEligibleCorps  = 1000
)

var corporationIDPattern = regexp.MustCompile(`^\d+$`)

// AccessTokenRecord is one EVE access token returned by auth-next.
type AccessTokenRecord struct {
	CharacterID   string    `json:"characterId"`
	CharacterName string    `json:"characterName"`
	UserID        uuid.UUID `json:"userId"`
	Role          string    `json:"role"`
	AccessToken   string    `json:"accessToken"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// CorporationListResponse is the validated list returned by auth-next.
type CorporationListResponse struct {
	CorporationIDs []string `json:"corporationIds"`
}

// CorporationTokenGroup contains the exported credentials for one corporation.
type CorporationTokenGroup struct {
	CorporationID   string              `json:"corporationId"`
	CorporationName string              `json:"corporationName"`
	Tokens          []AccessTokenRecord `json:"tokens"`
}

// CorporationError reports an independent export failure for one corporation.
type CorporationError struct {
	CorporationID string `json:"corporationId"`
	Message       string `json:"error"`
}

// Response is the validated result of a scoped token export request.
type Response struct {
	Corporations            []CorporationTokenGroup `json:"corporations"`
	Errors                  []CorporationError      `json:"errors"`
	RequestedCorporationIDs []string                `json:"requestedCorporationIds"`
}

// Client calls auth-next without exposing bearer credentials to callers.
type Client struct {
	baseURL     *url.URL
	bearerToken string
	httpClient  *http.Client
}

// NewClient creates a token-export client for an explicit auth-next base URL.
func NewClient(baseURL, bearerToken string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("auth-next token export base URL is required")
	}
	if strings.TrimSpace(bearerToken) == "" {
		return nil, errors.New("auth-next token export bearer token is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid auth-next token export base URL %q", baseURL)
	}
	validScheme := parsed.Scheme == "http" || parsed.Scheme == "https"
	if !validScheme || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid auth-next token export base URL %q", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: parsed, bearerToken: bearerToken, httpClient: httpClient}, nil
}

// ListCorporations retrieves the active member and special-purpose corporations.
func (c *Client) ListCorporations(ctx context.Context) ([]string, error) {
	var decoded CorporationListResponse
	if err := c.getJSON(ctx, corporationsPath, nil, &decoded); err != nil {
		return nil, err
	}
	if err := validateCorporationList(&decoded); err != nil {
		return nil, err
	}
	return decoded.CorporationIDs, nil
}

// FetchCorporation retrieves one corporation's token group.
func (c *Client) FetchCorporation(ctx context.Context, corporationID string) (Response, error) {
	if err := validateCorporationID(corporationID); err != nil {
		return Response{}, err
	}
	query := url.Values{}
	query.Set("corporationId", corporationID)
	var decoded Response
	if err := c.getJSON(ctx, exportPath, query, &decoded); err != nil {
		return Response{}, err
	}
	if err := validateResponse(&decoded); err != nil {
		return Response{}, err
	}
	return decoded, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, target any) error {
	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	if query != nil {
		requestURL.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("create token export request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request token export: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &HTTPError{
			StatusCode: response.StatusCode,
			Retryable: response.StatusCode == http.StatusRequestTimeout ||
				response.StatusCode == http.StatusTooManyRequests ||
				response.StatusCode >= http.StatusInternalServerError,
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read token export response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%w: response exceeds %d bytes", ErrInvalidResponse, maxResponseBytes)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%w: decode response: %w", ErrInvalidResponse, err)
	}
	return nil
}

func validateCorporationList(response *CorporationListResponse) error {
	if response.CorporationIDs == nil {
		return fmt.Errorf("%w: corporationIds is required", ErrInvalidResponse)
	}
	if len(response.CorporationIDs) > maxEligibleCorps {
		return fmt.Errorf("%w: response lists more than %d corporations", ErrInvalidResponse, maxEligibleCorps)
	}
	seen := make(map[string]struct{}, len(response.CorporationIDs))
	for i, corporationID := range response.CorporationIDs {
		if err := validateCorporationID(corporationID); err != nil {
			return fmt.Errorf("%w: corporation %d has invalid ID: %w", ErrInvalidResponse, i, err)
		}
		if _, exists := seen[corporationID]; exists {
			return fmt.Errorf("%w: duplicate corporation %s", ErrInvalidResponse, corporationID)
		}
		seen[corporationID] = struct{}{}
	}
	return nil
}

func validateResponse(response *Response) error {
	seen := make(map[string]struct{}, len(response.Corporations))
	for i := range response.Corporations {
		group := &response.Corporations[i]
		if err := validateGroup(group, i, seen); err != nil {
			return err
		}
		seen[group.CorporationID] = struct{}{}
	}
	for i := range response.Errors {
		if err := validateCorporationID(response.Errors[i].CorporationID); err != nil {
			return fmt.Errorf("%w: error %d has invalid corporation ID: %w", ErrInvalidResponse, i, err)
		}
	}
	if len(response.RequestedCorporationIDs) > maxRequestedCorps {
		return fmt.Errorf("%w: response requests more than %d corporations", ErrInvalidResponse, maxRequestedCorps)
	}
	for i, corporationID := range response.RequestedCorporationIDs {
		if err := validateCorporationID(corporationID); err != nil {
			return fmt.Errorf("%w: requested corporation %d has invalid ID: %w", ErrInvalidResponse, i, err)
		}
	}
	return nil
}

func validateGroup(group *CorporationTokenGroup, index int, seen map[string]struct{}) error {
	if err := validateCorporationID(group.CorporationID); err != nil {
		return fmt.Errorf("%w: corporation group %d: %w", ErrInvalidResponse, index, err)
	}
	if group.CorporationName == "" {
		return fmt.Errorf("%w: corporation %s has no name", ErrInvalidResponse, group.CorporationID)
	}
	if len(group.Tokens) > maxTokensPerCorp {
		return fmt.Errorf("%w: corporation %s has more than %d tokens", ErrInvalidResponse, group.CorporationID, maxTokensPerCorp)
	}
	if _, exists := seen[group.CorporationID]; exists {
		return fmt.Errorf("%w: duplicate corporation %s", ErrInvalidResponse, group.CorporationID)
	}
	characters := make(map[string]struct{}, len(group.Tokens))
	for tokenIndex := range group.Tokens {
		if err := validateToken(group.CorporationID, &group.Tokens[tokenIndex], tokenIndex, characters); err != nil {
			return err
		}
	}
	return nil
}

func validateToken(corporationID string, token *AccessTokenRecord, index int, seen map[string]struct{}) error {
	if err := validateCorporationID(token.CharacterID); err != nil {
		return fmt.Errorf("%w: corporation %s token %d: invalid character ID: %w", ErrInvalidResponse, corporationID, index, err)
	}
	if token.CharacterName == "" || token.UserID == uuid.Nil || token.AccessToken == "" || token.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: corporation %s token %d is incomplete", ErrInvalidResponse, corporationID, index)
	}
	if token.Role != "member" && token.Role != "director" {
		return fmt.Errorf("%w: corporation %s token %d has invalid role", ErrInvalidResponse, corporationID, index)
	}
	if _, exists := seen[token.CharacterID]; exists {
		return fmt.Errorf("%w: corporation %s has duplicate character %s", ErrInvalidResponse, corporationID, token.CharacterID)
	}
	seen[token.CharacterID] = struct{}{}
	return nil
}

func validateCorporationID(value string) error {
	if !corporationIDPattern.MatchString(value) {
		return errors.New("corporation ID must contain only digits")
	}
	return nil
}
