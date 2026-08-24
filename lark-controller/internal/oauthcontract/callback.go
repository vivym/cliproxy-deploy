package oauthcontract

import (
	"errors"
	"net/url"
)

const (
	ControllerCallbackPath = "/integrations/lark/oauth/callback"
	NewAPICallbackPath     = "/oauth/lark"
)

func DeriveControllerCallbackURI(newAPICallbackURI string) (string, error) {
	parsed, err := parseExactHTTPSCallback(newAPICallbackURI, NewAPICallbackPath)
	if err != nil {
		return "", errors.New("New API callback must be an exact HTTPS /oauth/lark URL")
	}
	origin := url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	return origin.String() + ControllerCallbackPath, nil
}

func ValidateControllerCallbackURI(callbackURI string) error {
	_, err := parseExactHTTPSCallback(callbackURI, ControllerCallbackPath)
	return err
}

func ValidateNewAPICallbackURI(callbackURI string) error {
	_, err := parseExactHTTPSCallback(callbackURI, NewAPICallbackPath)
	return err
}

func parseExactHTTPSCallback(raw, path string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.Path != path || parsed.RawPath != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("invalid OAuth callback URI")
	}
	return parsed, nil
}
