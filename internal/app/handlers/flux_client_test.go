package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFluxClientQueryParsesStreamedTables(t *testing.T) {
	var gotAuth, gotCT, gotQueryOrg, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotQueryOrg = r.URL.Query().Get("org")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/vnd.influxdb")
		// Annotated CSV exactly as the live influxd streams it: leading
		// empty annotation cell, header row per table, "_result" default.
		fmt.Fprint(w, ",result,table,_field,_value,endpoint\n")
		fmt.Fprint(w, ",_result,0,queue_depth,3,radiance-qfn\n")
		fmt.Fprint(w, ",_result,0,requests_running,2,radiance-qfn\n")
		fmt.Fprint(w, ",result,table,_field,_value,endpoint\n")
		fmt.Fprint(w, ",_result,1,queue_depth,9,halogen-395\n")
	}))
	defer srv.Close()

	c := newFluxClient(srv.URL, "org name", "tok123")
	rows, err := c.query(context.Background(), `from(bucket:"b")|>range(start:-5m)`)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, "Token tok123", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, "org name", gotQueryOrg, "org must travel as a URL query parameter")
	assert.Contains(t, gotBody, `"format":"csv"`, "the wire format the server actually speaks")
	assert.Contains(t, gotBody, `"header":true`)
	assert.Contains(t, gotBody, `"query":"from(bucket:\"b\")`)
	assert.Equal(t, "radiance-qfn", rows[0]["endpoint"])
	assert.Equal(t, "3", rows[0]["_value"], "CSV values are strings; call sites coerce")
	assert.Equal(t, "halogen-395", rows[2]["endpoint"])
}

func TestFluxClientQueryErrorTable(t *testing.T) {
	// Flux syntax errors arrive as a 200 + an "error" result table.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ",error,reference\n")
		fmt.Fprint(w, `,"error @1:168: found unexpected argument v",42`+"\n")
	}))
	defer srv.Close()
	c := newFluxClient(srv.URL, "org", "tok")
	_, err := c.query(context.Background(), "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found unexpected argument v")
}

func TestFluxClientQueryErrorExcerpt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provide orgID or org", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := newFluxClient(srv.URL, "org", "tok")
	_, err := c.query(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "provide orgID or org")
}

func TestFluxClientQueryTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	c := newFluxClient(srv.URL, "org", "tok")
	c.client.Timeout = 50 * time.Millisecond
	_, err := c.query(context.Background(), "x")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline"), "got %v", err)
}
