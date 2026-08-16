package restexecutor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/criyle/go-judge/session"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type sessionHandle struct {
	manager *session.Manager
	logger  *zap.Logger
}

func NewSessionHandle(manager *session.Manager, loggers ...*zap.Logger) Register {
	logger := zap.NewNop()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &sessionHandle{manager: manager, logger: logger}
}

func (h *sessionHandle) Register(r *gin.Engine) {
	if h.manager == nil {
		return
	}
	r.POST("/session", h.create)
	r.PUT("/session/:id/file/*filepath", h.writeFile)
	r.GET("/session/:id/file/*filepath", h.readFile)
	r.GET("/session/:id/files", h.listFiles)
	r.POST("/session/:id/exec", h.exec)
	r.GET("/session/:id/archive", h.archive)
	r.DELETE("/session/:id", h.delete)
}

func (h *sessionHandle) create(c *gin.Context) {
	var req session.CreateRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	s, err := h.manager.Create(req)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"sessionId": s.ID(),
		"createdAt": s.CreatedAt(),
	})
}

func (h *sessionHandle) writeFile(c *gin.Context) {
	s, err := h.lookup(c)
	if err != nil {
		return
	}
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	size, err := s.WriteFile(name, c.Request.Body)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "path": name, "size": size})
}

func (h *sessionHandle) readFile(c *gin.Context) {
	s, err := h.lookup(c)
	if err != nil {
		return
	}
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	f, err := s.OpenFile(name)
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer func() { _ = f.Close() }()
	c.DataFromReader(http.StatusOK, -1, "application/octet-stream", f, nil)
}

func (h *sessionHandle) listFiles(c *gin.Context) {
	s, err := h.lookup(c)
	if err != nil {
		return
	}
	files, err := s.ListFiles()
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": files})
}

func (h *sessionHandle) exec(c *gin.Context) {
	s, err := h.lookup(c)
	if err != nil {
		return
	}
	var req session.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	response, err := s.Exec(c.Request.Context(), req)
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *sessionHandle) archive(c *gin.Context) {
	s, err := h.lookup(c)
	if err != nil {
		return
	}
	archivePath, err := s.Archive(c.Query("pattern"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	defer os.Remove(archivePath)
	c.Header("Content-Type", "application/zip")
	c.File(archivePath)
}

func (h *sessionHandle) delete(c *gin.Context) {
	if err := h.manager.Delete(c.Param("id")); err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *sessionHandle) lookup(c *gin.Context) (*session.Session, error) {
	s, err := h.manager.Get(c.Param("id"))
	if err != nil {
		h.writeError(c, err)
	}
	return s, err
}

func (h *sessionHandle) writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, session.ErrFileNotFound):
		status = http.StatusNotFound
	case errors.Is(err, session.ErrInvalidPath), errors.Is(err, session.ErrInvalidRequest):
		status = http.StatusBadRequest
	case errors.Is(err, session.ErrDiskLimit):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, session.ErrSessionLimit):
		status = http.StatusTooManyRequests
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusRequestTimeout
	}
	message := err.Error()
	if errors.Is(err, session.ErrFileNotFound) {
		message = "File not found"
	}
	if status == http.StatusInternalServerError {
		if h.logger != nil {
			h.logger.Error("session request failed", zap.Error(err))
		}
		message = "Internal server error"
	}
	c.JSON(status, gin.H{"error": message})
}
