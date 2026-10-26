package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"autodocker-go/services"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// RegisterTerminalRoutes registers the WebSocket terminal endpoint.
func RegisterTerminalRoutes(r *gin.Engine) {
	r.GET("/ws/terminal", handleTerminalWS)
}

func handleTerminalWS(c *gin.Context) {
	// Check authentication
	session := sessions.Default(c)
	user := session.Get("user")
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Non authentifie"})
		return
	}

	cid := c.Query("container")
	if cid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "container parameter required"})
		return
	}

	cli := services.EnsureDocker()
	if cli == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Docker non connecte"})
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		fmt.Printf("[WS] Upgrade error: %v\n", err)
		return
	}
	defer conn.Close()

	ctx := context.Background()

	// Create exec
	execConfig := container.ExecOptions{
		Cmd:          []string{"/bin/sh"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}

	execResp, err := cli.ContainerExecCreate(ctx, cid, execConfig)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Erreur exec: %v\r\n", err)))
		return
	}

	// Attach to exec
	hijack, err := cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("Erreur attach: %v\r\n", err)))
		return
	}
	defer hijack.Close()

	// Read from Docker -> WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := hijack.Reader.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nConnexion terminee: %v\r\n", err)))
				}
				return
			}
		}
	}()

	// Read from WebSocket -> Docker
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if _, err := hijack.Conn.Write(msg); err != nil {
			break
		}
	}

	// Closing the Docker hijack unblocks the reader goroutine when the browser
	// closes the modal, otherwise stale exec sessions can linger.
	hijack.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
