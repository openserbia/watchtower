// Package auth handles registry authentication and bearer-token challenges.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	ref "github.com/distribution/reference"
	"github.com/sirupsen/logrus"

	"github.com/openserbia/watchtower/pkg/registry/helpers"
	"github.com/openserbia/watchtower/pkg/registry/retry"
	"github.com/openserbia/watchtower/pkg/registry/transport"
	"github.com/openserbia/watchtower/pkg/types"
)

// ChallengeHeader is the HTTP Header containing challenge instructions
const ChallengeHeader = "WWW-Authenticate"

// GetToken fetches a token for the registry hosting the provided image
func GetToken(container types.Container, registryAuth string) (string, error) {
	normalizedRef, err := ref.ParseNormalizedNamed(container.ImageName())
	if err != nil {
		return "", err
	}

	challengeURL := GetChallengeURL(normalizedRef)
	logrus.WithField("URL", challengeURL.String()).Debug("Built challenge URL")

	req, err := GetChallengeRequest(challengeURL)
	if err != nil {
		return "", err
	}

	client := transport.Client(challengeURL.Host)
	res, err := retry.DoHTTP(client, req, logrus.WithField("url", challengeURL.String()))
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	v := res.Header.Get(ChallengeHeader)

	logrus.WithFields(logrus.Fields{
		"status": res.Status,
		"header": v,
	}).Debug("Got response to challenge request")

	challenge := strings.ToLower(v)
	if strings.HasPrefix(challenge, "basic") {
		if registryAuth == "" {
			return "", errors.New("no credentials available")
		}

		return fmt.Sprintf("Basic %s", registryAuth), nil
	}
	if strings.HasPrefix(challenge, "bearer") {
		return GetBearerHeader(challenge, normalizedRef, registryAuth)
	}

	return "", errors.New("unsupported challenge type from registry")
}

// GetChallengeRequest creates a request for getting challenge instructions
func GetChallengeRequest(challengeURL url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, challengeURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Watchtower (Docker)")
	return req, nil
}

// GetBearerHeader tries to fetch a bearer token from the registry based on the challenge instructions
func GetBearerHeader(challenge string, imageRef ref.Named, registryAuth string) (string, error) {
	authURL, err := GetAuthURL(challenge, imageRef)
	if err != nil {
		return "", err
	}
	client := transport.Client(authURL.Host)

	authURLString := authURL.String()
	key := cacheKey(authURLString, registryAuth)
	if header, ok := bearerCache.get(key); ok {
		logrus.WithField("url", authURLString).Debug("Using cached bearer token")
		return header, nil
	}

	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authURLString, nil)
	if err != nil {
		return "", err
	}

	if registryAuth != "" {
		logrus.Debug("Credentials found.")
		// CREDENTIAL: Uncomment to log registry credentials
		// logrus.Tracef("Credentials: %v", registryAuth)
		r.Header.Add("Authorization", fmt.Sprintf("Basic %s", registryAuth))
	} else {
		logrus.Debug("No credentials found.")
	}

	authResponse, err := retry.DoHTTP(client, r, logrus.WithField("url", authURLString))
	if err != nil {
		return "", err
	}
	defer func() { _ = authResponse.Body.Close() }()

	body, _ := io.ReadAll(authResponse.Body)
	tokenResponse := &types.TokenResponse{}

	err = json.Unmarshal(body, tokenResponse)
	if err != nil {
		return "", err
	}

	header := fmt.Sprintf("Bearer %s", tokenResponse.Token)
	ttl := defaultTokenTTL
	if tokenResponse.ExpiresIn > 0 {
		ttl = time.Duration(tokenResponse.ExpiresIn) * time.Second
	}
	bearerCache.set(key, header, ttl)

	return header, nil
}

// GetAuthURL from the instructions in the challenge
func GetAuthURL(challenge string, imageRef ref.Named) (*url.URL, error) {
	loweredChallenge := strings.ToLower(challenge)
	raw := strings.TrimPrefix(loweredChallenge, "bearer")

	pairs := strings.Split(raw, ",")
	values := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		trimmed := strings.Trim(pair, " ")
		if key, val, ok := strings.Cut(trimmed, "="); ok {
			values[key] = strings.Trim(val, `"`)
		}
	}
	logrus.WithFields(logrus.Fields{
		"realm":   values["realm"],
		"service": values["service"],
	}).Debug("Checking challenge header content")
	if values["realm"] == "" || values["service"] == "" {
		return nil, errors.New("challenge header did not include all values needed to construct an auth url")
	}

	authURL, _ := url.Parse(values["realm"])
	q := authURL.Query()
	q.Add("service", values["service"])

	scopeImage := ref.Path(imageRef)

	scope := fmt.Sprintf("repository:%s:pull", scopeImage)
	logrus.WithFields(logrus.Fields{"scope": scope, "image": imageRef.Name()}).Debug("Setting scope for auth token")
	q.Add("scope", scope)

	authURL.RawQuery = q.Encode()
	return authURL, nil
}

// GetChallengeURL returns the URL to check auth requirements
// for access to a given image
func GetChallengeURL(imageRef ref.Named) url.URL {
	host, _ := helpers.GetRegistryAddress(imageRef.Name())

	return url.URL{
		Scheme: "https",
		Host:   host,
		Path:   "/v2/",
	}
}
