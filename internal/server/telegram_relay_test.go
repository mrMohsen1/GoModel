package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type telegramRelayRoundTripper func(*http.Request) (*http.Response, error)

func (f telegramRelayRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTelegramRelayForwardsBotAPICall(t *testing.T) {
	const botToken = "123456:REAL_BOT_TOKEN"
	const clientToken = "relay-client-secret"
	var gotMethod, gotHost, gotPath, gotQuery, gotBody, gotAuthorization string

	relay := newTelegramRelayHandlerWithTransport(botToken, clientToken, telegramRelayRoundTripper(func(request *http.Request) (*http.Response, error) {
		gotMethod = request.Method
		gotHost = request.URL.Host
		gotPath = request.URL.Path
		gotQuery = request.URL.RawQuery
		gotAuthorization = request.Header.Get("Authorization")
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("{\"ok\":true,\"result\":true}")),
		}, nil
	}))

	request := httptest.NewRequest(http.MethodPost, "/telegram/bot"+clientToken+"/sendMessage?parse_mode=HTML", strings.NewReader("{\"chat_id\":1,\"text\":\"hello\"}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer should-not-be-forwarded")
	recorder := httptest.NewRecorder()

	relay.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "api.telegram.org", gotHost)
	assert.Equal(t, "/bot"+botToken+"/sendMessage", gotPath)
	assert.Equal(t, "parse_mode=HTML", gotQuery)
	assert.Empty(t, gotAuthorization)
	assert.JSONEq(t, "{\"chat_id\":1,\"text\":\"hello\"}", gotBody)
	assert.JSONEq(t, "{\"ok\":true,\"result\":true}", recorder.Body.String())
}

func TestTelegramRelayForwardsFileDownload(t *testing.T) {
	const botToken = "123456:REAL_BOT_TOKEN"
	const clientToken = "relay-client-secret"
	var gotPath string

	relay := newTelegramRelayHandlerWithTransport(botToken, clientToken, telegramRelayRoundTripper(func(request *http.Request) (*http.Response, error) {
		gotPath = request.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
			Body:       io.NopCloser(strings.NewReader("file-bytes")),
		}, nil
	}))

	request := httptest.NewRequest(http.MethodGet, "/telegram/file/bot"+clientToken+"/photos/file_123.jpg", nil)
	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "/file/bot"+botToken+"/photos/file_123.jpg", gotPath)
	assert.Equal(t, "image/jpeg", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "file-bytes", recorder.Body.String())
}

func TestTelegramRelayRejectsUnexpectedClientToken(t *testing.T) {
	called := false
	relay := newTelegramRelayHandlerWithTransport("123456:real", "expected-client", telegramRelayRoundTripper(func(request *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	}))

	request := httptest.NewRequest(http.MethodGet, "/telegram/botwrong-client/getMe", nil)
	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.False(t, called)
}

func TestTelegramRelayRejectsUnsafePaths(t *testing.T) {
	const botToken = "123456:REAL_BOT_TOKEN"
	const clientToken = "relay-client-secret"
	relay := newTelegramRelayHandlerWithTransport(botToken, clientToken, telegramRelayRoundTripper(func(request *http.Request) (*http.Response, error) {
		t.Fatal("upstream must not be called for an unsafe path")
		return nil, nil
	}))

	for _, requestPath := range []string{
		"/telegram/bot" + clientToken + "/sendMessage/extra",
		"/telegram/file/bot" + clientToken + "/../secret",
		"/telegram/file/bot" + clientToken + "/photos//file.jpg",
	} {
		t.Run(requestPath, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, requestPath, nil)
			recorder := httptest.NewRecorder()
			relay.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusNotFound, recorder.Code)
		})
	}
}

func TestRedactTelegramRelayPath(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			input: "/telegram/botrelay-client-secret/sendMessage",
			want:  "/telegram/botREDACTED/sendMessage",
		},
		{
			input: "/telegram/file/botrelay-client-secret/photos/file.jpg",
			want:  "/telegram/file/botREDACTED/photos/file.jpg",
		},
		{
			input: "/v1/chat/completions",
			want:  "/v1/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, redactTelegramRelayPath(tc.input))
		})
	}
}

func TestRedactTelegramRelayURI(t *testing.T) {
	assert.Equal(t,
		"/telegram/botREDACTED/getUpdates?offset=42",
		redactTelegramRelayURI("/telegram/botrelay-client-secret/getUpdates?offset=42"),
	)
}
