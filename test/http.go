package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime/pprof"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resgateio/resgate/server/reserr"
)

// HTTPRequest represents a HTTP requests made to the gateway
type HTTPRequest struct {
	ch  chan *HTTPResponse
	req *http.Request
	rr  *httptest.ResponseRecorder
}

// HTTPResponse represents a response received from a HTTP request
// made to the gateway
type HTTPResponse struct {
	*httptest.ResponseRecorder
}

// GetResponse awaits for a response and returns it.
// Fails if a response hasn't arrived within 1 second.
func (hr *HTTPRequest) GetResponse(t *testing.T) *HTTPResponse {
	select {
	case resp := <-hr.ch:
		return resp
	case <-time.After(timeoutSeconds * time.Second):
		if t == nil {
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			panic(fmt.Sprintf("expected a response to http request %#v, but found none", hr.req.URL.Path))
		} else {
			t.Fatalf("expected a response to http request %#v, but found none", hr.req.URL.Path)
		}
	}
	return nil
}

// Equals asserts that the response has the expected status code and body
func (hr *HTTPResponse) Equals(t *testing.T, code int, body interface{}) *HTTPResponse {
	hr.AssertStatusCode(t, code)
	hr.AssertBody(t, body)
	return hr
}

// AssertStatusCode asserts that the response has the expected status code
func (hr *HTTPResponse) AssertStatusCode(t *testing.T, code int) *HTTPResponse {
	if hr.Code != code {
		t.Fatalf("expected response code to be %d, but got %d", code, hr.Code)
	}
	return hr
}

// AssertBody asserts that the response has the expected body
func (hr *HTTPResponse) AssertBody(t *testing.T, body interface{}) *HTTPResponse {
	var err error
	var bj []byte

	f := func(e, a string) {
		if len(e) == 0 {
			t.Fatalf("expected response to be empty, but got:\n%s", a)
		} else if len(a) == 0 {
			t.Fatalf("expected response body to be:\n%s\nbut got empty response", e)
		} else {
			t.Fatalf("expected response body to be:\n%s\nbut got:\n%s", e, a)
		}
	}

	actual := hr.Body.String()

	// Check if we have an exact string
	if bj, ok := body.([]byte); ok {
		if strings.TrimSpace(actual) != string(bj) {
			f(string(bj), actual)
		}
		return hr
	}

	var ab interface{}
	if body != nil {
		bj, err = json.Marshal(body)
		if err != nil {
			panic("test: error marshaling assertion body: " + err.Error())
		}

		err = json.Unmarshal(bj, &ab)
		if err != nil {
			panic("test: error unmarshaling assertion body: " + err.Error())
		}
	}

	bb := hr.Body.Bytes()
	// Quick exit if both are empty
	if len(bb) == 0 && body == nil {
		return hr
	}

	var b interface{}
	err = json.Unmarshal(bb, &b)
	if err != nil {
		f(string(bj), actual)
	}

	if !reflect.DeepEqual(ab, b) {
		f(string(bj), actual)
	}
	return hr
}

// AssertError asserts that the response does not have status 200, and has
// the expected error
func (hr *HTTPResponse) AssertError(t *testing.T, err *reserr.Error) *HTTPResponse {
	if hr.Code == http.StatusOK {
		t.Fatalf("expected response code not to be 200, but it was")
	}
	hr.AssertBody(t, err)
	return hr
}

// AssertErrorCode asserts that the response does not have status 200, and
// has the expected error code
func (hr *HTTPResponse) AssertErrorCode(t *testing.T, code string) *HTTPResponse {
	if hr.Code == http.StatusOK {
		t.Fatalf("expected response code not to be 200, but it was")
	}

	var rerr reserr.Error
	err := json.Unmarshal(hr.Body.Bytes(), &rerr)
	if err != nil {
		t.Fatalf("expected error response, but got body:\n%s", hr.Body.String())
	}

	if rerr.Code != code {
		t.Fatalf("expected response error code to be:\n%#v\nbut got:\n%#v", code, rerr.Code)
	}
	return hr
}

// AssertIsError asserts that the response does not have status 200,
// and that the body has an error code.
func (hr *HTTPResponse) AssertIsError(t *testing.T) *HTTPResponse {
	if hr.Code == http.StatusOK {
		t.Fatalf("expected response code not to be 200, but it was")
	}

	var rerr reserr.Error
	err := json.Unmarshal(hr.Body.Bytes(), &rerr)
	if err != nil || rerr.Code == "" {
		t.Fatalf("expected error response, but got body:\n%s", hr.Body.String())
	}
	return hr
}

// AssertHeaders asserts that the response includes the expected headers
func (hr *HTTPResponse) AssertHeaders(t *testing.T, h map[string]string) *HTTPResponse {
	for k, v := range h {
		hv := hr.Result().Header.Get(k)
		if hv != v {
			if hv == "" {
				t.Fatalf("expected response header %s to be %s, but header not found", k, v)
			} else {
				t.Fatalf("expected response header %s to be %s, but got %s", k, v, hv)
			}
		}
	}
	return hr
}

// AssertMultiHeaders asserts that the response includes the expected headers, including repeated headers such as Set-Cookie.
func (hr *HTTPResponse) AssertMultiHeaders(t *testing.T, h map[string][]string) *HTTPResponse {
	for k, v := range h {
		hv := hr.Result().Header[k]
		sort.StringSlice(hv).Sort()
		sort.StringSlice(v).Sort()
		if !reflect.DeepEqual(hv, v) {
			if len(hv) == 0 {
				t.Fatalf("expected response header %s to be:\n\t%#v\nbut header not found", k, v)
			} else if len(v) == 0 {
				t.Fatalf("expected response header %s to be missing, but got:\n\t%#v", k, hv)
			} else {
				t.Fatalf("expected response header %s to be:\n\t%#v\nbut got:\n\t%#v", k, v, hv)
			}
		}
	}
	return hr
}

// AssertMissingHeaders asserts that the response does not include the given headers
func (hr *HTTPResponse) AssertMissingHeaders(t *testing.T, h []string) *HTTPResponse {
	for _, h := range h {
		hv := hr.Result().Header.Get(h)
		if hv != "" {
			t.Fatalf("expected response header %s to be missing, but got %s", h, hv)
		}
	}
	return hr
}
