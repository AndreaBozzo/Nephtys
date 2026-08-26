package connector

import "net/url"

// redactURL renders an endpoint for an error message without its credentials.
//
// Connector errors name the endpoint that failed, and those errors reach
// GET /v1/streams through a stream's last_error. Endpoint URLs routinely carry
// a token in the query string or in userinfo, and neither is needed to say
// which endpoint went wrong.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// Not parseable as a URL: return the host-free form rather than
		// guessing which part of it might be a secret.
		return "the configured endpoint"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
