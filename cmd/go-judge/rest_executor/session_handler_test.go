package restexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/criyle/go-judge/env/pool"
	"github.com/criyle/go-judge/envexec"
	"github.com/criyle/go-judge/session"
	"github.com/gin-gonic/gin"
)

type handlerFakeBuilder struct{}

func (handlerFakeBuilder) BuildWorkspace(string) (pool.Environment, error) {
	return &handlerFakeEnvironment{}, nil
}

type handlerFakeEnvironment struct{}

func (*handlerFakeEnvironment) Execve(context.Context, envexec.ExecveParam) (envexec.Process, error) {
	return nil, errors.New("not implemented")
}
func (*handlerFakeEnvironment) Open([]envexec.OpenParam) ([]envexec.OpenResult, error) {
	return nil, nil
}
func (*handlerFakeEnvironment) Symlink([]envexec.SymlinkParam) ([]error, error) { return nil, nil }
func (*handlerFakeEnvironment) Reset() error                                    { return nil }
func (*handlerFakeEnvironment) Destroy() error                                  { return nil }

func TestSessionRESTFileFlow(t *testing.T) {
	m, err := session.NewManager(session.Config{
		Root:           t.TempDir(),
		Builder:        handlerFakeBuilder{},
		DefaultTTL:     time.Hour,
		DefaultMaxDisk: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	r := gin.New()
	NewSessionHandle(m).Register(r)

	create := httptest.NewRecorder()
	r.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/session", bytes.NewBufferString(`{}`)))
	if create.Code != http.StatusOK {
		t.Fatalf("create status: %d %s", create.Code, create.Body.String())
	}
	var created struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("invalid create response: %s", create.Body.String())
	}

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/session/"+created.ID+"/file/gen.py", bytes.NewBufferString("print(1)")))
	if put.Code != http.StatusOK {
		t.Fatalf("put status: %d %s", put.Code, put.Body.String())
	}

	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/session/"+created.ID+"/file/gen.py", nil))
	if get.Code != http.StatusOK || get.Body.String() != "print(1)" {
		t.Fatalf("get response: %d %q", get.Code, get.Body.String())
	}
	if got := get.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content type: %q", got)
	}

	missing := httptest.NewRecorder()
	r.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/session/"+created.ID+"/file/missing", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "File not found") {
		t.Fatalf("missing response: %d %s", missing.Code, missing.Body.String())
	}

	archive := httptest.NewRecorder()
	r.ServeHTTP(archive, httptest.NewRequest(http.MethodGet, "/session/"+created.ID+"/archive?pattern=*.py", nil))
	if archive.Code != http.StatusOK || archive.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("archive response: %d %q", archive.Code, archive.Header().Get("Content-Type"))
	}

}
