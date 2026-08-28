package server

import (
	"net/url"
	"regexp"
	"strings"

	"nephtys/internal/domain"
)

// This file holds the rules about what the API is allowed to hand back. They
// live together because they answer one question — which operator-supplied
// bytes leave the process over HTTP — and because getting one of them wrong is
// not visible from the endpoint that uses it.

// redactedValue stands in for any value the API withholds. It is deliberately
// neither empty nor a plausible value: a reader has to be able to tell a field
// that is set and withheld from one that was never configured.
const redactedValue = "[REDACTED]"

// redactConfig returns cfg with every value that could be a credential
// withheld, keeping the structure that names it.
//
// GET /v1/streams/{id} is the first endpoint to serve a stream's configuration
// back, so it is the first that could be used to read a credential that was
// only ever written. What it withholds is what a stream presents to something
// else in order to be let in: an auth token, a header value, a connect frame,
// and the parts of a URL that carry either. What it keeps is structure — kind,
// topic, ports, the path this process serves, intervals, the restart block —
// and values that are never presented as credentials by any code path:
// metadata, which nothing reads at all, and the pipeline, whose enrich tags are
// already broadcast on every event Nephtys publishes. Serving those back
// exposes nothing that was not already out.
//
// Where the two overlap, syntax decides. A separator that tells a label from a
// value lets the label through: a header keeps its name, a query parameter
// keeps its key. Where nothing separates them the whole thing goes, which is
// why a URL path segment is withheld even though most of them are routes —
// /v2/stream/recentchange and /services/T00/B00/xoxb-secret are the same shape.
//
// A name-based denylist was the obvious alternative and is unenforceable: the
// header carrying a token is called Authorization by convention only, and
// nothing stops an operator from calling it X-Thing. The cost of deciding by
// syntax instead is that Accept: application/json is withheld too, which is a
// small price for a rule that cannot be defeated by naming something
// differently.
//
// The result is a description of a running stream, not a document that can be
// POSTed back.
func redactConfig(cfg domain.StreamSourceConfig) domain.StreamSourceConfig {
	// cfg is a copy, but its pointer and map fields still alias the manager's
	// stored config — the one that gets persisted and restarted. Every branch
	// below therefore rewrites through a fresh copy: mutating in place would
	// rewrite the running stream into the redacted version of itself, and the
	// damage would only surface at the next restart.
	cfg.URL = redactURL(cfg.URL)

	if cfg.Webhook != nil {
		webhook := *cfg.Webhook
		if webhook.AuthToken != "" {
			webhook.AuthToken = redactedValue
		}
		cfg.Webhook = &webhook
	}

	if cfg.Sse != nil {
		sse := *cfg.Sse
		sse.Headers = redactHeaders(sse.Headers)
		cfg.Sse = &sse
	}

	if cfg.RestPoller != nil {
		poller := *cfg.RestPoller
		poller.Headers = redactHeaders(poller.Headers)
		cfg.RestPoller = &poller
	}

	if cfg.Websocket != nil {
		websocket := *cfg.Websocket
		websocket.OnConnectSend = redactFrames(websocket.OnConnectSend)
		cfg.Websocket = &websocket
	}

	return cfg
}

// redactHeaders copies a header map with every value withheld. The names stay:
// which headers a stream sends is the thing an operator is checking, and it is
// not the secret.
func redactHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	redacted := make(map[string]string, len(headers))
	for name := range headers {
		redacted[name] = redactedValue
	}
	return redacted
}

// redactFrames withholds on_connect_send frames whole, keeping how many there
// are and the order they go out in. A frame is a verbatim application message
// and is where a venue's auth handshake goes, so there is no part of one that
// can be shown safely — but a stream sending two frames instead of one is a
// difference worth being able to see.
func redactFrames(frames domain.StringList) domain.StringList {
	if len(frames) == 0 {
		return frames
	}
	redacted := make(domain.StringList, len(frames))
	for i := range redacted {
		redacted[i] = redactedValue
	}
	return redacted
}

// redactURL withholds the parts of a URL that could be a credential while
// keeping the part that names the endpoint.
//
// The line is drawn by syntax, exactly as it is for headers: where a separator
// tells a label apart from a value, the label stays; where nothing does, the
// whole thing goes.
//
//	scheme://host:port  kept — structural, and names the service being talked to
//	userinfo            dropped — a user:password@ pair has no diagnostic value
//	path                each segment withheld, count and shape kept
//	query "k=v"         key kept, value withheld
//	query bare flag     withheld whole — no "=", so it may be a value
//	fragment            dropped
//
// Path segments go because nothing separates a route from a token inside one:
// /v2/stream/recentchange and /services/T00/B00/xoxb-secret are the same shape,
// and a URL-embedded token is how a whole class of webhook and feed endpoints
// authenticates. Keeping the host is what stops this from being a blanket
// redaction — an operator can still see which service a stream talks to, and
// which query parameters it sends.
//
// This is deliberately not redactForAPI, which drops query strings whole and
// leaves paths alone. That one edits free-form error text, where a partially
// rewritten URL cannot be reassembled reliably; this one edits a structured
// field, and is the only one of the two whose output an operator reads as a
// description of a configuration.
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		// A URL we cannot take apart is one we cannot redact. Withhold it
		// whole rather than guessing at where its parts end.
		return redactedValue
	}

	parsed.User = nil
	// Path and RawPath are set to the same string so String() emits it
	// verbatim: left to escape Path on its own it would render every marker as
	// %5BREDACTED%5D, which is correct and unreadable.
	redactedPath := redactPathSegments(parsed.Path)
	parsed.Path = redactedPath
	parsed.RawPath = redactedPath
	parsed.RawQuery = redactQueryValues(parsed.RawQuery)
	parsed.Fragment = ""
	return parsed.String()
}

// redactPathSegments withholds each segment of a path, keeping how many there
// are and any trailing slash. Two segments instead of five is a difference an
// operator can act on; which two they are is not something we can serve.
func redactPathSegments(path string) string {
	if path == "" || path == "/" {
		return path
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		// Splitting a rooted path yields an empty leading element, and a
		// trailing slash yields an empty final one. Both are structure.
		if segment != "" {
			segments[i] = redactedValue
		}
	}
	return strings.Join(segments, "/")
}

// redactQueryValues rewrites a raw query string, keeping each key and the order
// they appear in and withholding every value. It works on the raw string rather
// than url.Values because that type sorts keys and collapses nothing — a config
// echo that reorders the query on every read is a poor thing to diff.
func redactQueryValues(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}

	params := strings.Split(rawQuery, "&")
	for i, param := range params {
		key, _, hasValue := strings.Cut(param, "=")
		if !hasValue {
			// No "=" means nothing marks this as a label, so it may be a bare
			// token rather than a flag. Withhold it whole; "there is one
			// valueless parameter here" is all that can be said safely.
			if param != "" {
				params[i] = redactedValue
			}
			continue
		}
		params[i] = key + "=" + redactedValue
	}
	return strings.Join(params, "&")
}

// urlInMessage matches a URL embedded in an error string. Connector errors
// quote the configured endpoint, and net/http quotes it for us in url.Error.
var urlInMessage = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"']+`)

// redactForAPI strips credentials from any URL inside an error message.
//
// last_error is served by GET /v1/streams, whose auth is optional, so it is a
// path from connector errors to whoever can reach the API. Endpoint URLs
// routinely carry tokens in a query string or in userinfo, and neither is
// needed to tell an operator which endpoint failed. The unredacted error still
// goes to the log, which is not served over HTTP.
func redactForAPI(message string) string {
	return urlInMessage.ReplaceAllStringFunc(message, func(raw string) string {
		// Error text tends to end a URL with punctuation. Keep it out of the
		// parse and put it back afterwards.
		trimmed := strings.TrimRight(raw, `.,;:!?)]}`)
		suffix := raw[len(trimmed):]

		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			return raw
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String() + suffix
	})
}
